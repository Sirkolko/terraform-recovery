package discovery

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

func tags(kv ...string) []ec2types.Tag {
	var out []ec2types.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, ec2types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return out
}

var denied = &smithy.GenericAPIError{Code: "AccessDenied", Message: "not authorized to perform rds:DescribeDBInstances"}

type fakeEC2 struct{}

func (fakeEC2) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{Vpcs: []ec2types.Vpc{
		{VpcId: aws.String("vpc-default"), CidrBlock: aws.String("172.31.0.0/16"), IsDefault: aws.Bool(true), InstanceTenancy: "default"},
		{VpcId: aws.String("vpc-prod"), CidrBlock: aws.String("10.0.0.0/16"), IsDefault: aws.Bool(false), InstanceTenancy: "default", Tags: tags("Name", "prod")},
	}}, nil
}

func (fakeEC2) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{
		{SubnetId: aws.String("subnet-a"), VpcId: aws.String("vpc-prod"), CidrBlock: aws.String("10.0.1.0/24"),
			AvailabilityZone: aws.String("eu-west-1a"), MapPublicIpOnLaunch: aws.Bool(true), Tags: tags("Name", "public-a")},
		{SubnetId: aws.String("subnet-def"), VpcId: aws.String("vpc-default"), CidrBlock: aws.String("172.31.0.0/20"), DefaultForAz: aws.Bool(true)},
	}}, nil
}

func (fakeEC2) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{RouteTables: []ec2types.RouteTable{
		{RouteTableId: aws.String("rtb-main"), VpcId: aws.String("vpc-prod"),
			Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(true)}}},
		{RouteTableId: aws.String("rtb-public"), VpcId: aws.String("vpc-prod"), Tags: tags("Name", "public"),
			Routes: []ec2types.Route{
				{DestinationCidrBlock: aws.String("10.0.0.0/16"), GatewayId: aws.String("local"), Origin: ec2types.RouteOriginCreateRouteTable},
				{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-prod"), Origin: ec2types.RouteOriginCreateRoute},
			},
			Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-a")}}},
	}}, nil
}

func (fakeEC2) DescribeInternetGateways(context.Context, *ec2.DescribeInternetGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: []ec2types.InternetGateway{
		{InternetGatewayId: aws.String("igw-prod"), Attachments: []ec2types.InternetGatewayAttachment{{VpcId: aws.String("vpc-prod")}}},
		{InternetGatewayId: aws.String("igw-default"), Attachments: []ec2types.InternetGatewayAttachment{{VpcId: aws.String("vpc-default")}}},
	}}, nil
}

func (fakeEC2) DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{NatGateways: []ec2types.NatGateway{
		{NatGatewayId: aws.String("nat-live"), State: ec2types.NatGatewayStateAvailable, SubnetId: aws.String("subnet-a"),
			NatGatewayAddresses: []ec2types.NatGatewayAddress{{AllocationId: aws.String("eipalloc-1")}}},
		{NatGatewayId: aws.String("nat-gone"), State: ec2types.NatGatewayStateDeleted},
	}}, nil
}

func (fakeEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{
		{GroupId: aws.String("sg-default"), GroupName: aws.String("default"), VpcId: aws.String("vpc-prod"), Description: aws.String("default VPC security group")},
		{GroupId: aws.String("sg-web"), GroupName: aws.String("web"), VpcId: aws.String("vpc-prod"), Description: aws.String("Managed by Terraform"),
			IpPermissions: []ec2types.IpPermission{{UserIdGroupPairs: []ec2types.UserIdGroupPair{{GroupId: aws.String("sg-lb")}}}}},
	}}, nil
}

func (fakeEC2) DescribeSecurityGroupRules(context.Context, *ec2.DescribeSecurityGroupRulesInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	return &ec2.DescribeSecurityGroupRulesOutput{SecurityGroupRules: []ec2types.SecurityGroupRule{
		{SecurityGroupRuleId: aws.String("sgr-1"), GroupId: aws.String("sg-web"), IsEgress: aws.Bool(false), IpProtocol: aws.String("tcp"),
			FromPort: aws.Int32(443), ToPort: aws.Int32(443), CidrIpv4: aws.String("0.0.0.0/0")},
	}}, nil
}

func (fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
		{InstanceId: aws.String("i-web"), InstanceType: "t3.micro", ImageId: aws.String("ami-1"), SubnetId: aws.String("subnet-a"),
			VpcId: aws.String("vpc-prod"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			RootDeviceName: aws.String("/dev/xvda"), Tags: tags("Name", "web"),
			SecurityGroups:     []ec2types.GroupIdentifier{{GroupId: aws.String("sg-web")}},
			IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("arn:aws:iam::111122223333:instance-profile/app/web-profile")},
			BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{
				{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root")}},
			}},
		{InstanceId: aws.String("i-dead"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameTerminated}},
	}}}}, nil
}

func (fakeEC2) DescribeVolumes(context.Context, *ec2.DescribeVolumesInput, ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: []ec2types.Volume{
		{VolumeId: aws.String("vol-root"), Size: aws.Int32(8), VolumeType: "gp3", AvailabilityZone: aws.String("eu-west-1a"),
			Attachments: []ec2types.VolumeAttachment{{InstanceId: aws.String("i-web"), Device: aws.String("/dev/xvda")}}},
		{VolumeId: aws.String("vol-data"), Size: aws.Int32(100), VolumeType: "gp3", AvailabilityZone: aws.String("eu-west-1a"),
			Attachments: []ec2types.VolumeAttachment{{InstanceId: aws.String("i-web"), Device: aws.String("/dev/sdf")}}},
	}}, nil
}

func (fakeEC2) DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{Addresses: []ec2types.Address{
		{AllocationId: aws.String("eipalloc-1"), PublicIp: aws.String("203.0.113.10"), Domain: ec2types.DomainTypeVpc},
	}}, nil
}

func (fakeEC2) DescribeKeyPairs(context.Context, *ec2.DescribeKeyPairsInput, ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error) {
	return &ec2.DescribeKeyPairsOutput{KeyPairs: []ec2types.KeyPairInfo{{KeyName: aws.String("deployer"), KeyPairId: aws.String("key-1")}}}, nil
}

func (fakeEC2) DescribeRegions(context.Context, *ec2.DescribeRegionsInput, ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error) {
	return &ec2.DescribeRegionsOutput{Regions: []ec2types.Region{{RegionName: aws.String("eu-west-1")}, {RegionName: aws.String("us-east-1")}}}, nil
}

type fakeELB struct{}

const (
	lbARN = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:loadbalancer/app/web/abc"
	tgARN = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:targetgroup/web/def"
	lsARN = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:listener/app/web/abc/ghi"
)

func (fakeELB) DescribeLoadBalancers(context.Context, *elb.DescribeLoadBalancersInput, ...func(*elb.Options)) (*elb.DescribeLoadBalancersOutput, error) {
	return &elb.DescribeLoadBalancersOutput{LoadBalancers: []elbtypes.LoadBalancer{{
		LoadBalancerArn: aws.String(lbARN), LoadBalancerName: aws.String("web"), Type: elbtypes.LoadBalancerTypeEnumApplication,
		Scheme: elbtypes.LoadBalancerSchemeEnumInternetFacing, VpcId: aws.String("vpc-prod"),
		AvailabilityZones: []elbtypes.AvailabilityZone{{SubnetId: aws.String("subnet-a")}}, SecurityGroups: []string{"sg-lb"},
	}}}, nil
}

func (fakeELB) DescribeTargetGroups(context.Context, *elb.DescribeTargetGroupsInput, ...func(*elb.Options)) (*elb.DescribeTargetGroupsOutput, error) {
	return &elb.DescribeTargetGroupsOutput{TargetGroups: []elbtypes.TargetGroup{{
		TargetGroupArn: aws.String(tgARN), TargetGroupName: aws.String("web"), Port: aws.Int32(80),
		Protocol: elbtypes.ProtocolEnumHttp, TargetType: elbtypes.TargetTypeEnumInstance, VpcId: aws.String("vpc-prod"),
	}}}, nil
}

func (fakeELB) DescribeListeners(context.Context, *elb.DescribeListenersInput, ...func(*elb.Options)) (*elb.DescribeListenersOutput, error) {
	return &elb.DescribeListenersOutput{Listeners: []elbtypes.Listener{{
		ListenerArn: aws.String(lsARN), LoadBalancerArn: aws.String(lbARN), Port: aws.Int32(443), Protocol: elbtypes.ProtocolEnumHttps,
		DefaultActions: []elbtypes.Action{{Type: elbtypes.ActionTypeEnumForward, TargetGroupArn: aws.String(tgARN)}},
	}}}, nil
}

func (fakeELB) DescribeTags(_ context.Context, in *elb.DescribeTagsInput, _ ...func(*elb.Options)) (*elb.DescribeTagsOutput, error) {
	var out []elbtypes.TagDescription
	for _, arn := range in.ResourceArns {
		out = append(out, elbtypes.TagDescription{ResourceArn: aws.String(arn), Tags: []elbtypes.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}}})
	}
	return &elb.DescribeTagsOutput{TagDescriptions: out}, nil
}

type fakeRDS struct{}

func (fakeRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput, ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	return nil, denied
}

func (fakeRDS) DescribeDBSubnetGroups(context.Context, *rds.DescribeDBSubnetGroupsInput, ...func(*rds.Options)) (*rds.DescribeDBSubnetGroupsOutput, error) {
	return &rds.DescribeDBSubnetGroupsOutput{}, nil
}

type fakeS3 struct{}

func (fakeS3) ListBuckets(context.Context, *s3.ListBucketsInput, ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	return &s3.ListBucketsOutput{Buckets: []s3types.Bucket{
		{Name: aws.String("acme-logs"), BucketRegion: aws.String("eu-west-1")},
		{Name: aws.String("acme-us"), BucketRegion: aws.String("us-east-1")},
		{Name: aws.String("acme-old")},
	}}, nil
}

func (fakeS3) GetBucketLocation(context.Context, *s3.GetBucketLocationInput, ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error) {
	return &s3.GetBucketLocationOutput{LocationConstraint: "EU"}, nil
}

func (fakeS3) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	if aws.ToString(in.Bucket) == "acme-old" {
		return nil, &smithy.GenericAPIError{Code: "NoSuchTagSet"}
	}
	return &s3.GetBucketTaggingOutput{TagSet: []s3types.Tag{{Key: aws.String("Team"), Value: aws.String("data")}}}, nil
}

type fakeIAM struct{}

func (fakeIAM) ListRoles(context.Context, *iam.ListRolesInput, ...func(*iam.Options)) (*iam.ListRolesOutput, error) {
	return &iam.ListRolesOutput{Roles: []iamtypes.Role{
		{RoleName: aws.String("app"), Arn: aws.String("arn:aws:iam::111122223333:role/app"), Path: aws.String("/"), RoleId: aws.String("AROA1")},
		{RoleName: aws.String("AWSServiceRoleForRDS"), Arn: aws.String("arn:aws:iam::111122223333:role/aws-service-role/rds.amazonaws.com/AWSServiceRoleForRDS"),
			Path: aws.String("/aws-service-role/rds.amazonaws.com/")},
	}}, nil
}

func (fakeIAM) ListPolicies(context.Context, *iam.ListPoliciesInput, ...func(*iam.Options)) (*iam.ListPoliciesOutput, error) {
	return &iam.ListPoliciesOutput{Policies: []iamtypes.Policy{{PolicyName: aws.String("app-read"), Arn: aws.String("arn:aws:iam::111122223333:policy/app-read"), Path: aws.String("/")}}}, nil
}

func (fakeIAM) ListUsers(context.Context, *iam.ListUsersInput, ...func(*iam.Options)) (*iam.ListUsersOutput, error) {
	return &iam.ListUsersOutput{}, nil
}

func (fakeIAM) ListInstanceProfiles(context.Context, *iam.ListInstanceProfilesInput, ...func(*iam.Options)) (*iam.ListInstanceProfilesOutput, error) {
	return &iam.ListInstanceProfilesOutput{InstanceProfiles: []iamtypes.InstanceProfile{{
		InstanceProfileName: aws.String("web-profile"), Arn: aws.String("arn:aws:iam::111122223333:instance-profile/app/web-profile"),
		Path: aws.String("/app/"), Roles: []iamtypes.Role{{RoleName: aws.String("app")}},
	}}}, nil
}

type fakeSTS struct{}

func (fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("111122223333"), Arn: aws.String("arn:aws:sts::111122223333:assumed-role/ReadOnly/alice")}, nil
}

type fakeClients struct{}

func (fakeClients) EC2(string) ec2API { return fakeEC2{} }
func (fakeClients) ELB(string) elbAPI { return fakeELB{} }
func (fakeClients) RDS(string) rdsAPI { return fakeRDS{} }
func (fakeClients) S3(string) s3API   { return fakeS3{} }
func (fakeClients) IAM(string) iamAPI { return fakeIAM{} }
func (fakeClients) STS(string) stsAPI { return fakeSTS{} }

func fakeScanner() *Scanner {
	return &Scanner{clients: fakeClients{}, defaultRegion: "eu-west-1", logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), concurrency: 4}
}

func index(inv *models.Inventory) map[string]*models.CloudResource {
	out := map[string]*models.CloudResource{}
	for _, r := range inv.Resources {
		out[r.ID] = r
	}
	return out
}

func TestScan(t *testing.T) {
	inv, err := fakeScanner().Scan(context.Background(), []string{"eu-west-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inv.AccountID != "111122223333" || inv.Partition != "aws" {
		t.Errorf("identity = %s %s", inv.AccountID, inv.Partition)
	}
	res := index(inv)
	for _, id := range []string{"nat-gone", "i-dead", "acme-us"} {
		if res[id] != nil {
			t.Errorf("%s must not be in the inventory", id)
		}
	}
	autoIgnored := map[string]string{
		"vpc-default": "Default VPC", "subnet-def": "Default subnet", "igw-default": "default VPC",
		"rtb-main": "Main route table", "sg-default": "Default security group", "vol-root": "Root volume of i-web",
		"AWSServiceRoleForRDS": "service-linked", "sgr-1": "Security group rule",
	}
	for id, reason := range autoIgnored {
		if r := res[id]; r == nil || !strings.Contains(r.AutoIgnore, reason) {
			t.Errorf("%s auto-ignore = %+v, want %q", id, r, reason)
		}
	}
	for _, id := range []string{"vpc-prod", "subnet-a", "rtb-public", "igw-prod", "sg-web", "i-web", "vol-data", "eipalloc-1", "deployer", "acme-logs", "acme-old", "app"} {
		if r := res[id]; r == nil || r.AutoIgnore != "" {
			t.Errorf("%s should be a regular resource: %+v", id, r)
		}
	}

	rt := res["rtb-public"]
	if !reflect.DeepEqual(rt.Relations["route.gateway_id"], []string{"igw-prod"}) || !reflect.DeepEqual(rt.Attributes["route.cidr_block"], []string{"0.0.0.0/0"}) {
		t.Errorf("route table = %+v", rt)
	}
	if got := res["i-web"].Relations["iam_instance_profile"]; !reflect.DeepEqual(got, []string{"arn:aws:iam::111122223333:instance-profile/app/web-profile", "web-profile"}) {
		t.Errorf("instance profile relation = %v", got)
	}
	if lb := res[lbARN]; lb == nil || lb.Tags["Env"] != "prod" || lb.Attributes["internal"][0] != "false" || lb.Name != "web" {
		t.Errorf("load balancer = %+v", lb)
	}
	if l := res[lsARN]; l == nil || l.Name != "web:443" || l.Relations["default_action.target_group_arn"][0] != tgARN {
		t.Errorf("listener = %+v", l)
	}
	if b := res["acme-old"]; b.Region != "eu-west-1" || len(b.Tags) != 0 {
		t.Errorf("bucket from GetBucketLocation = %+v", b)
	}
	if b := res["acme-logs"]; b.Tags["Team"] != "data" || b.ARN != "arn:aws:s3:::acme-logs" {
		t.Errorf("bucket = %+v", b)
	}

	var sawDenied, sawSkipped bool
	for _, c := range inv.Coverage {
		if c.Service == "rds:DescribeDBInstances" && c.Status == models.CoverageDenied && strings.Contains(c.Error, "AccessDenied") {
			sawDenied = true
		}
		if c.Service == "s3:ListAllMyBuckets" && strings.Contains(c.Error, "1 bucket(s)") {
			sawSkipped = true
		}
	}
	if !sawDenied || !sawSkipped {
		t.Errorf("coverage = %+v", inv.Coverage)
	}
}

func TestInventoryRoundTrip(t *testing.T) {
	inv, err := fakeScanner().Scan(context.Background(), []string{"eu-west-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub", "inventory.json")
	if err := SaveInventory(path, inv); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("inventory permissions = %v", info.Mode().Perm())
	}
	back, err := LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Resources) != len(inv.Resources) || back.AccountID != inv.AccountID {
		t.Errorf("round trip lost data")
	}
}

func TestProfilesReadsOnlySectionNames(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	creds := filepath.Join(dir, "credentials")
	os.WriteFile(cfg, []byte("[default]\nregion = eu-west-1\n[profile prod]\nsso_session = corp\n[sso-session corp]\nsso_region = eu-west-1\n"), 0o600)
	os.WriteFile(creds, []byte("[default]\naws_access_key_id = AKIAEXAMPLE\n[legacy]\naws_secret_access_key = x\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	if got := Profiles(); !reflect.DeepEqual(got, []string{"default", "legacy", "prod"}) {
		t.Errorf("profiles = %v", got)
	}
}
