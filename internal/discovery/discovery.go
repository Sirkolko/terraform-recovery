// Package discovery lists existing AWS resources using read-only API calls
// (Describe*, List*, Get*). It never modifies anything in the account and
// relies on the AWS SDK default credential chain, so no credentials are ever
// entered into, read by, or stored by this tool.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

const defaultConcurrency = 8

// Identity describes the AWS caller.
type Identity struct {
	AccountID string `json:"account_id"`
	ARN       string `json:"arn"`
	UserID    string `json:"user_id"`
	Partition string `json:"partition"`
	Profile   string `json:"profile,omitempty"`
}

// Scanner discovers resources in one AWS account.
type Scanner struct {
	clients       clientFactory
	profile       string
	defaultRegion string
	logger        *slog.Logger
	concurrency   int
}

// NewScanner loads the AWS configuration through the SDK default credential
// chain: environment variables, shared config and credentials files
// (including SSO and assumed roles), and container or instance metadata.
func NewScanner(ctx context.Context, profile string, logger *slog.Logger) (*Scanner, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(8),
		config.WithAppID("terraform-recovery"),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{
		clients:       newSDKClients(cfg),
		profile:       profile,
		defaultRegion: cfg.Region,
		logger:        logger,
		concurrency:   defaultConcurrency,
	}, nil
}

// DefaultRegion returns the region configured for the profile or
// environment, if any.
func (s *Scanner) DefaultRegion() string { return s.defaultRegion }

// Identity calls sts:GetCallerIdentity.
func (s *Scanner) Identity(ctx context.Context) (*Identity, error) {
	out, err := s.clients.STS(s.homeRegion(nil)).GetCallerIdentity(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("AWS authentication failed: %s", credentialHint(err))
	}
	id := &Identity{
		AccountID: aws.ToString(out.Account),
		ARN:       aws.ToString(out.Arn),
		UserID:    aws.ToString(out.UserId),
		Partition: "aws",
		Profile:   s.profile,
	}
	if parts := strings.SplitN(id.ARN, ":", 3); len(parts) >= 2 && parts[0] == "arn" {
		id.Partition = parts[1]
	}
	return id, nil
}

// EnabledRegions lists the regions enabled for the account.
func (s *Scanner) EnabledRegions(ctx context.Context) ([]string, error) {
	out, err := s.clients.EC2(s.homeRegion(nil)).DescribeRegions(ctx, nil)
	if err != nil {
		return nil, err
	}
	var regions []string
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	sort.Strings(regions)
	return regions, nil
}

// homeRegion picks the region used for global services (STS, IAM, S3
// listing): the configured default, else the first scanned region.
func (s *Scanner) homeRegion(regions []string) string {
	if s.defaultRegion != "" {
		return s.defaultRegion
	}
	if len(regions) > 0 {
		return regions[0]
	}
	return "us-east-1"
}

// credentialHint turns common credential errors into actionable messages.
func credentialHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SSO") || strings.Contains(msg, "sso"):
		return msg + " — if you use IAM Identity Center, run `aws sso login` for this profile and retry"
	case strings.Contains(msg, "no EC2 IMDS role found") || strings.Contains(msg, "failed to retrieve credentials"):
		return msg + " — configure credentials with `aws configure`, a profile, or environment variables"
	case strings.Contains(msg, "ExpiredToken"):
		return msg + " — the session token has expired; refresh your credentials"
	}
	return msg
}

// collector discovers one kind of resource.
type collector struct {
	service string // e.g. ec2:DescribeVpcs
	resType string
	global  bool
	fn      func(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error)
}

// scanContext carries what a collector needs for one region.
type scanContext struct {
	s         *Scanner
	region    string
	regions   []string
	account   string
	partition string
	// notes are additional coverage entries (e.g. partially denied calls).
	notes []models.Coverage
	mu    sync.Mutex
}

func (sc *scanContext) note(c models.Coverage) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.notes = append(sc.notes, c)
}

func (sc *scanContext) arn(service, region, resource string) string {
	return fmt.Sprintf("arn:%s:%s:%s:%s:%s", sc.partition, service, region, sc.account, resource)
}

var collectors = []collector{
	{service: "ec2:DescribeVpcs", resType: "aws_vpc", fn: collectVPCs},
	{service: "ec2:DescribeSubnets", resType: "aws_subnet", fn: collectSubnets},
	{service: "ec2:DescribeRouteTables", resType: "aws_route_table", fn: collectRouteTables},
	{service: "ec2:DescribeInternetGateways", resType: "aws_internet_gateway", fn: collectInternetGateways},
	{service: "ec2:DescribeNatGateways", resType: "aws_nat_gateway", fn: collectNatGateways},
	{service: "ec2:DescribeSecurityGroups", resType: "aws_security_group", fn: collectSecurityGroups},
	{service: "ec2:DescribeSecurityGroupRules", resType: "aws_vpc_security_group_rule", fn: collectSecurityGroupRules},
	{service: "ec2:DescribeInstances", resType: "aws_instance", fn: collectInstances},
	{service: "ec2:DescribeVolumes", resType: "aws_ebs_volume", fn: collectVolumes},
	{service: "ec2:DescribeAddresses", resType: "aws_eip", fn: collectAddresses},
	{service: "ec2:DescribeKeyPairs", resType: "aws_key_pair", fn: collectKeyPairs},
	{service: "elasticloadbalancing:DescribeLoadBalancers", resType: "aws_lb", fn: collectLoadBalancing},
	{service: "rds:DescribeDBInstances", resType: "aws_db_instance", fn: collectDBInstances},
	{service: "rds:DescribeDBSubnetGroups", resType: "aws_db_subnet_group", fn: collectDBSubnetGroups},
	{service: "s3:ListAllMyBuckets", resType: "aws_s3_bucket", global: true, fn: collectBuckets},
	{service: "iam:ListRoles", resType: "aws_iam_role", global: true, fn: collectRoles},
	{service: "iam:ListPolicies", resType: "aws_iam_policy", global: true, fn: collectPolicies},
	{service: "iam:ListUsers", resType: "aws_iam_user", global: true, fn: collectUsers},
	{service: "iam:ListInstanceProfiles", resType: "aws_iam_instance_profile", global: true, fn: collectInstanceProfiles},
}

// Scan discovers all supported resources in the given regions plus global
// services. Failures of individual API calls are recorded in the coverage
// report instead of aborting the scan.
func (s *Scanner) Scan(ctx context.Context, regions []string, progress func(string)) (*models.Inventory, error) {
	if len(regions) == 0 {
		if s.defaultRegion == "" {
			return nil, errors.New("no region selected and no default region configured")
		}
		regions = []string{s.defaultRegion}
	}
	regions = dedupeSorted(regions)
	if progress == nil {
		progress = func(string) {}
	}
	id, err := s.Identity(ctx)
	if err != nil {
		return nil, err
	}
	progress(fmt.Sprintf("Authenticated as %s (account %s)", id.ARN, id.AccountID))
	inv := &models.Inventory{
		Version:   models.InventoryVersion,
		ScannedAt: time.Now().UTC(),
		AccountID: id.AccountID,
		CallerARN: id.ARN,
		Partition: id.Partition,
		Profile:   s.profile,
		Regions:   regions,
	}

	type task struct {
		c      collector
		region string
	}
	var tasks []task
	for _, c := range collectors {
		if c.global {
			tasks = append(tasks, task{c, "global"})
			continue
		}
		for _, r := range regions {
			tasks = append(tasks, task{c, r})
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, s.concurrency))
	for _, tk := range tasks {
		wg.Add(1)
		go func(tk task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sc := &scanContext{s: s, region: tk.region, regions: regions, account: id.AccountID, partition: id.Partition}
			if tk.region == "global" {
				sc.region = s.homeRegion(regions)
			}
			start := time.Now()
			res, err := tk.c.fn(ctx, sc)
			cov := models.Coverage{Region: tk.region, Service: tk.c.service, Type: tk.c.resType, Count: len(res), Status: models.CoverageOK}
			if err != nil {
				cov.Status, cov.Error = classify(err)
				s.logger.Warn("discovery call failed", "service", tk.c.service, "region", tk.region, "status", cov.Status)
			}
			progress(fmt.Sprintf("%s %s: %d found (%s) in %s", tk.region, tk.c.service, len(res), cov.Status, time.Since(start).Round(time.Millisecond)))
			mu.Lock()
			defer mu.Unlock()
			inv.Resources = append(inv.Resources, res...)
			inv.Coverage = append(inv.Coverage, cov)
			inv.Coverage = append(inv.Coverage, sc.notes...)
		}(tk)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	Finalize(inv)
	return inv, nil
}

// classify maps an API error to a coverage status and message.
func classify(err error) (string, string) {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		msg := code
		if m := ae.ErrorMessage(); m != "" {
			msg += ": " + m
		}
		if isAccessDenied(code) {
			return models.CoverageDenied, msg
		}
		return models.CoverageError, msg
	}
	if errors.Is(err, context.Canceled) {
		return models.CoverageError, "cancelled"
	}
	return models.CoverageError, err.Error()
}

func isAccessDenied(code string) bool {
	switch code {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "AuthorizationError",
		"UnauthorizedAccess", "Forbidden", "AccessDeniedFault":
		return true
	}
	return strings.Contains(code, "AccessDenied")
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// --- resource helpers ----------------------------------------------------------

func newResource(resType, region, id, account string) *models.CloudResource {
	return &models.CloudResource{
		Key:         models.CloudKey(resType, region, id),
		Type:        resType,
		ID:          id,
		ImportID:    id,
		Region:      region,
		AccountID:   account,
		Tags:        map[string]string{},
		Attributes:  map[string][]string{},
		Relations:   map[string][]string{},
		Identifiers: map[string]string{"id": id},
	}
}

func setAttr(r *models.CloudResource, attr string, values ...string) {
	for _, v := range values {
		if v != "" {
			r.Attributes[attr] = append(r.Attributes[attr], v)
		}
	}
}

func setRel(r *models.CloudResource, attr string, values ...string) {
	for _, v := range values {
		if v != "" && !contains(r.Relations[attr], v) {
			r.Relations[attr] = append(r.Relations[attr], v)
		}
	}
}

func setBool(r *models.CloudResource, attr string, v *bool) {
	if v != nil {
		setAttr(r, attr, fmt.Sprint(*v))
	}
}

func setInt(r *models.CloudResource, attr string, v *int32) {
	if v != nil {
		setAttr(r, attr, fmt.Sprint(*v))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func withName(r *models.CloudResource, fallback string) {
	r.Name = r.Tags["Name"]
	if r.Name == "" {
		r.Name = fallback
	}
}

// Finalize derives cross-resource facts after discovery: which resources
// belong to default VPCs, which volumes are instance root volumes, and so on.
// It marks resources that are normally not managed with Terraform so they can
// be ignored when nothing in the configuration matches them.
func Finalize(inv *models.Inventory) {
	defaultVPCs := map[string]bool{}
	rootDevices := map[string]string{} // instance ID -> root device name
	for _, r := range inv.Resources {
		switch r.Type {
		case "aws_vpc":
			if v, _ := r.Attr("is_default"); v == "true" {
				defaultVPCs[r.ID] = true
			}
		case "aws_instance":
			if d, _ := r.Attr("root_device_name"); d != "" {
				rootDevices[r.ID] = d
			}
		}
	}
	inDefaultVPC := func(r *models.CloudResource) bool {
		for _, v := range r.Relations["vpc_id"] {
			if defaultVPCs[v] {
				return true
			}
		}
		return false
	}
	for _, r := range inv.Resources {
		if r.AutoIgnore != "" {
			continue
		}
		switch r.Type {
		case "aws_internet_gateway":
			if inDefaultVPC(r) {
				r.AutoIgnore = "Internet gateway of the default VPC"
			}
		case "aws_route_table":
			main, _ := r.Attr("main")
			switch {
			case main == "true" && inDefaultVPC(r):
				r.AutoIgnore = "Main route table of the default VPC"
			case main == "true" && r.Tags["Name"] == "":
				r.AutoIgnore = "Main route table created with the VPC (manage it with aws_default_route_table if needed)"
			}
		case "aws_security_group":
			if n, _ := r.Attr("name"); n == "default" {
				r.AutoIgnore = "Default security group of " + strings.Join(r.Relations["vpc_id"], ", ") + " (manage it with aws_default_security_group if needed)"
			}
		case "aws_vpc_security_group_rule":
			r.AutoIgnore = "Security group rule — usually managed inline by aws_security_group or by aws_vpc_security_group_*_rule resources"
		case "aws_ebs_volume":
			for _, inst := range r.Relations["attachment.instance_id"] {
				dev, _ := r.Attr("attachment.device")
				if root := rootDevices[inst]; root != "" && root == dev {
					setAttr(r, "root_device_of", inst)
					r.AutoIgnore = "Root volume of " + inst + " (managed through aws_instance)"
				}
			}
		}
	}
	inv.Sort()
}
