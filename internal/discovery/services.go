package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// --- Elastic Load Balancing ------------------------------------------------------

// collectLoadBalancing discovers load balancers, target groups and
// listeners, then fetches their tags in batches.
func collectLoadBalancing(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	client := sc.s.clients.ELB(sc.region)
	var out []*models.CloudResource
	byARN := map[string]*models.CloudResource{}

	lbs := elb.NewDescribeLoadBalancersPaginator(client, &elb.DescribeLoadBalancersInput{})
	var lbARNs []string
	for lbs.HasMorePages() {
		page, err := lbs.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, lb := range page.LoadBalancers {
			arn := aws.ToString(lb.LoadBalancerArn)
			r := newResource("aws_lb", sc.region, arn, sc.account)
			r.ARN = arn
			r.Name = aws.ToString(lb.LoadBalancerName)
			r.Identifiers["arn"] = arn
			r.Identifiers["name"] = r.Name
			if i := strings.Index(arn, ":loadbalancer/"); i >= 0 {
				r.Identifiers["arn_suffix"] = arn[i+len(":loadbalancer/"):]
			}
			setAttr(r, "name", r.Name)
			setAttr(r, "load_balancer_type", string(lb.Type))
			setAttr(r, "internal", fmt.Sprint(lb.Scheme == elbtypes.LoadBalancerSchemeEnumInternal))
			setAttr(r, "ip_address_type", string(lb.IpAddressType))
			setAttr(r, "dns_name", aws.ToString(lb.DNSName))
			setRel(r, "vpc_id", aws.ToString(lb.VpcId))
			for _, az := range lb.AvailabilityZones {
				setRel(r, "subnets", aws.ToString(az.SubnetId))
			}
			setRel(r, "security_groups", lb.SecurityGroups...)
			out = append(out, r)
			byARN[arn] = r
			lbARNs = append(lbARNs, arn)
		}
	}

	tgs := elb.NewDescribeTargetGroupsPaginator(client, &elb.DescribeTargetGroupsInput{})
	for tgs.HasMorePages() {
		page, err := tgs.NextPage(ctx)
		if err != nil {
			sc.note(models.Coverage{Region: sc.region, Service: "elasticloadbalancing:DescribeTargetGroups", Type: "aws_lb_target_group", Status: statusOf(err), Error: messageOf(err)})
			break
		}
		for _, tg := range page.TargetGroups {
			arn := aws.ToString(tg.TargetGroupArn)
			r := newResource("aws_lb_target_group", sc.region, arn, sc.account)
			r.ARN = arn
			r.Name = aws.ToString(tg.TargetGroupName)
			r.Identifiers["arn"] = arn
			r.Identifiers["name"] = r.Name
			setAttr(r, "name", r.Name)
			setInt(r, "port", tg.Port)
			setAttr(r, "protocol", string(tg.Protocol))
			setAttr(r, "target_type", string(tg.TargetType))
			setAttr(r, "health_check.path", aws.ToString(tg.HealthCheckPath))
			setRel(r, "vpc_id", aws.ToString(tg.VpcId))
			setRel(r, "load_balancer_arns", tg.LoadBalancerArns...)
			out = append(out, r)
			byARN[arn] = r
		}
	}

	for _, lbARN := range lbARNs {
		ls := elb.NewDescribeListenersPaginator(client, &elb.DescribeListenersInput{LoadBalancerArn: aws.String(lbARN)})
		for ls.HasMorePages() {
			page, err := ls.NextPage(ctx)
			if err != nil {
				sc.note(models.Coverage{Region: sc.region, Service: "elasticloadbalancing:DescribeListeners", Type: "aws_lb_listener", Status: statusOf(err), Error: messageOf(err)})
				break
			}
			for _, l := range page.Listeners {
				arn := aws.ToString(l.ListenerArn)
				r := newResource("aws_lb_listener", sc.region, arn, sc.account)
				r.ARN = arn
				r.Identifiers["arn"] = arn
				r.Name = fmt.Sprintf("%s:%d", byARN[lbARN].Name, aws.ToInt32(l.Port))
				setInt(r, "port", l.Port)
				setAttr(r, "protocol", string(l.Protocol))
				setRel(r, "load_balancer_arn", aws.ToString(l.LoadBalancerArn))
				for _, a := range l.DefaultActions {
					setRel(r, "default_action.target_group_arn", aws.ToString(a.TargetGroupArn))
					if a.ForwardConfig != nil {
						for _, tg := range a.ForwardConfig.TargetGroups {
							setRel(r, "default_action.target_group_arn", aws.ToString(tg.TargetGroupArn))
						}
					}
				}
				out = append(out, r)
				byARN[arn] = r
			}
		}
	}

	// DescribeTags accepts at most 20 ARNs per call.
	var arns []string
	for _, r := range out {
		arns = append(arns, r.ARN)
	}
	for i := 0; i < len(arns); i += 20 {
		end := min(i+20, len(arns))
		page, err := client.DescribeTags(ctx, &elb.DescribeTagsInput{ResourceArns: arns[i:end]})
		if err != nil {
			sc.note(models.Coverage{Region: sc.region, Service: "elasticloadbalancing:DescribeTags", Type: "aws_lb", Status: statusOf(err), Error: messageOf(err) + " (tags of load balancing resources are missing)"})
			break
		}
		for _, td := range page.TagDescriptions {
			r := byARN[aws.ToString(td.ResourceArn)]
			if r == nil {
				continue
			}
			for _, t := range td.Tags {
				r.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}
	return out, nil
}

// --- RDS -------------------------------------------------------------------------------

func collectDBInstances(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := rds.NewDescribeDBInstancesPaginator(sc.s.clients.RDS(sc.region), &rds.DescribeDBInstancesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, db := range page.DBInstances {
			if aws.ToString(db.DBInstanceStatus) == "deleting" {
				continue
			}
			ident := aws.ToString(db.DBInstanceIdentifier)
			r := newResource("aws_db_instance", sc.region, ident, sc.account)
			r.Name = ident
			r.ARN = aws.ToString(db.DBInstanceArn)
			r.Identifiers["arn"] = r.ARN
			r.Identifiers["identifier"] = ident
			// Since AWS provider v5 the id attribute is the DBI resource ID.
			r.Identifiers["id"] = aws.ToString(db.DbiResourceId)
			r.Identifiers["resource_id"] = aws.ToString(db.DbiResourceId)
			for _, t := range db.TagList {
				r.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			setAttr(r, "identifier", ident)
			setAttr(r, "engine", aws.ToString(db.Engine))
			setAttr(r, "engine_version", aws.ToString(db.EngineVersion))
			setAttr(r, "instance_class", aws.ToString(db.DBInstanceClass))
			setInt(r, "allocated_storage", db.AllocatedStorage)
			setAttr(r, "storage_type", aws.ToString(db.StorageType))
			setBool(r, "multi_az", db.MultiAZ)
			if db.Endpoint != nil {
				setInt(r, "port", db.Endpoint.Port)
			}
			setAttr(r, "username", aws.ToString(db.MasterUsername))
			setAttr(r, "db_name", aws.ToString(db.DBName))
			setAttr(r, "cluster_identifier", aws.ToString(db.DBClusterIdentifier))
			if db.DBSubnetGroup != nil {
				setRel(r, "db_subnet_group_name", aws.ToString(db.DBSubnetGroup.DBSubnetGroupName))
				setRel(r, "vpc_id", aws.ToString(db.DBSubnetGroup.VpcId))
			}
			for _, g := range db.VpcSecurityGroups {
				setRel(r, "vpc_security_group_ids", aws.ToString(g.VpcSecurityGroupId))
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectDBSubnetGroups(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := rds.NewDescribeDBSubnetGroupsPaginator(sc.s.clients.RDS(sc.region), &rds.DescribeDBSubnetGroupsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, g := range page.DBSubnetGroups {
			name := aws.ToString(g.DBSubnetGroupName)
			r := newResource("aws_db_subnet_group", sc.region, name, sc.account)
			r.Name = name
			r.ARN = aws.ToString(g.DBSubnetGroupArn)
			r.Identifiers["arn"] = r.ARN
			r.Identifiers["name"] = name
			setAttr(r, "name", name)
			setAttr(r, "description", aws.ToString(g.DBSubnetGroupDescription))
			setRel(r, "vpc_id", aws.ToString(g.VpcId))
			for _, s := range g.Subnets {
				setRel(r, "subnet_ids", aws.ToString(s.SubnetIdentifier))
			}
			if name == "default" {
				r.AutoIgnore = "Default DB subnet group created by AWS"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// --- S3 -------------------------------------------------------------------------------

func normalizeBucketRegion(loc string) string {
	switch loc {
	case "":
		return "us-east-1"
	case "EU":
		return "eu-west-1"
	}
	return loc
}

// collectBuckets lists buckets (a global call) and keeps those located in
// the scanned regions. Tags are read from each bucket's own region.
func collectBuckets(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	client := sc.s.clients.S3(sc.region)
	var out []*models.CloudResource
	skipped := 0
	p := s3.NewListBucketsPaginator(client, &s3.ListBucketsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, b := range page.Buckets {
			name := aws.ToString(b.Name)
			region := aws.ToString(b.BucketRegion)
			if region == "" {
				loc, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(name)})
				if err != nil {
					sc.note(models.Coverage{Region: "global", Service: "s3:GetBucketLocation", Type: "aws_s3_bucket", Status: statusOf(err), Error: name + ": " + messageOf(err)})
					continue
				}
				region = normalizeBucketRegion(string(loc.LocationConstraint))
			}
			if !contains(sc.regions, region) {
				skipped++
				continue
			}
			r := newResource("aws_s3_bucket", region, name, sc.account)
			r.Name = name
			r.ARN = fmt.Sprintf("arn:%s:s3:::%s", sc.partition, name)
			r.Identifiers["arn"] = r.ARN
			r.Identifiers["bucket"] = name
			setAttr(r, "bucket", name)
			out = append(out, r)
		}
	}
	if skipped > 0 {
		sc.note(models.Coverage{Region: "global", Service: "s3:ListAllMyBuckets", Type: "aws_s3_bucket", Status: models.CoverageOK,
			Error: fmt.Sprintf("%d bucket(s) in regions that were not scanned are not included", skipped)})
	}
	fetchBucketTags(ctx, sc, out)
	return out, nil
}

func fetchBucketTags(ctx context.Context, sc *scanContext, buckets []*models.CloudResource) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	denied := 0
	for _, b := range buckets {
		wg.Add(1)
		go func(b *models.CloudResource) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := sc.s.clients.S3(b.Region).GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(b.ID)})
			if err != nil {
				var ae smithy.APIError
				if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchTagSet" {
					return
				}
				mu.Lock()
				denied++
				mu.Unlock()
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, t := range out.TagSet {
				b.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}(b)
	}
	wg.Wait()
	if denied > 0 {
		sc.note(models.Coverage{Region: "global", Service: "s3:GetBucketTagging", Type: "aws_s3_bucket", Status: models.CoverageDenied,
			Error: fmt.Sprintf("tags of %d bucket(s) could not be read", denied)})
	}
}

// --- IAM (global) ---------------------------------------------------------------------

func iamResource(sc *scanContext, resType, id, name, arn string) *models.CloudResource {
	r := newResource(resType, "global", id, sc.account)
	r.Name = name
	r.ARN = arn
	r.Identifiers["arn"] = arn
	r.Identifiers["name"] = name
	setAttr(r, "name", name)
	return r
}

func collectRoles(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := iam.NewListRolesPaginator(sc.s.clients.IAM(sc.region), &iam.ListRolesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, role := range page.Roles {
			name := aws.ToString(role.RoleName)
			r := iamResource(sc, "aws_iam_role", name, name, aws.ToString(role.Arn))
			r.Identifiers["unique_id"] = aws.ToString(role.RoleId)
			path := aws.ToString(role.Path)
			setAttr(r, "path", path)
			setAttr(r, "description", aws.ToString(role.Description))
			setInt(r, "max_session_duration", role.MaxSessionDuration)
			switch {
			case strings.HasPrefix(path, "/aws-service-role/"):
				r.AutoIgnore = "AWS service-linked role"
			case strings.HasPrefix(path, "/aws-reserved/"):
				r.AutoIgnore = "Role reserved by AWS (for example IAM Identity Center)"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectPolicies(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := iam.NewListPoliciesPaginator(sc.s.clients.IAM(sc.region), &iam.ListPoliciesInput{Scope: iamtypes.PolicyScopeTypeLocal})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, pol := range page.Policies {
			arn := aws.ToString(pol.Arn)
			r := iamResource(sc, "aws_iam_policy", arn, aws.ToString(pol.PolicyName), arn)
			r.Identifiers["policy_id"] = aws.ToString(pol.PolicyId)
			setAttr(r, "path", aws.ToString(pol.Path))
			setAttr(r, "description", aws.ToString(pol.Description))
			out = append(out, r)
		}
	}
	return out, nil
}

func collectUsers(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := iam.NewListUsersPaginator(sc.s.clients.IAM(sc.region), &iam.ListUsersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, u := range page.Users {
			name := aws.ToString(u.UserName)
			r := iamResource(sc, "aws_iam_user", name, name, aws.ToString(u.Arn))
			r.Identifiers["unique_id"] = aws.ToString(u.UserId)
			setAttr(r, "path", aws.ToString(u.Path))
			out = append(out, r)
		}
	}
	return out, nil
}

func collectInstanceProfiles(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := iam.NewListInstanceProfilesPaginator(sc.s.clients.IAM(sc.region), &iam.ListInstanceProfilesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, ip := range page.InstanceProfiles {
			name := aws.ToString(ip.InstanceProfileName)
			r := iamResource(sc, "aws_iam_instance_profile", name, name, aws.ToString(ip.Arn))
			r.Identifiers["unique_id"] = aws.ToString(ip.InstanceProfileId)
			setAttr(r, "path", aws.ToString(ip.Path))
			for _, role := range ip.Roles {
				setRel(r, "role", aws.ToString(role.RoleName))
			}
			if strings.HasPrefix(aws.ToString(ip.Path), "/aws-service-role/") {
				r.AutoIgnore = "Instance profile managed by AWS"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func statusOf(err error) string {
	s, _ := classify(err)
	return s
}

func messageOf(err error) string {
	_, m := classify(err)
	return m
}
