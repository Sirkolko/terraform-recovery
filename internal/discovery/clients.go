package discovery

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// The interfaces below list every AWS API operation this tool can call.
// All of them are read-only; there is deliberately no way to reach a
// mutating operation through them.

type ec2API interface {
	ec2.DescribeVpcsAPIClient
	ec2.DescribeSubnetsAPIClient
	ec2.DescribeRouteTablesAPIClient
	ec2.DescribeInternetGatewaysAPIClient
	ec2.DescribeNatGatewaysAPIClient
	ec2.DescribeSecurityGroupsAPIClient
	ec2.DescribeSecurityGroupRulesAPIClient
	ec2.DescribeInstancesAPIClient
	ec2.DescribeVolumesAPIClient
	DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeKeyPairs(context.Context, *ec2.DescribeKeyPairsInput, ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error)
	DescribeRegions(context.Context, *ec2.DescribeRegionsInput, ...func(*ec2.Options)) (*ec2.DescribeRegionsOutput, error)
}

type elbAPI interface {
	elb.DescribeLoadBalancersAPIClient
	elb.DescribeTargetGroupsAPIClient
	elb.DescribeListenersAPIClient
	DescribeTags(context.Context, *elb.DescribeTagsInput, ...func(*elb.Options)) (*elb.DescribeTagsOutput, error)
}

type rdsAPI interface {
	rds.DescribeDBInstancesAPIClient
	rds.DescribeDBSubnetGroupsAPIClient
}

type s3API interface {
	s3.ListBucketsAPIClient
	GetBucketLocation(context.Context, *s3.GetBucketLocationInput, ...func(*s3.Options)) (*s3.GetBucketLocationOutput, error)
	GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
}

type iamAPI interface {
	iam.ListRolesAPIClient
	iam.ListPoliciesAPIClient
	iam.ListUsersAPIClient
	iam.ListInstanceProfilesAPIClient
}

type stsAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// clientFactory creates regional clients. Tests substitute fakes.
type clientFactory interface {
	EC2(region string) ec2API
	ELB(region string) elbAPI
	RDS(region string) rdsAPI
	S3(region string) s3API
	IAM(region string) iamAPI
	STS(region string) stsAPI
}

type sdkClients struct {
	cfg aws.Config
	mu  sync.Mutex
	ec2 map[string]*ec2.Client
	elb map[string]*elb.Client
	rds map[string]*rds.Client
	s3  map[string]*s3.Client
	iam map[string]*iam.Client
	sts map[string]*sts.Client
}

func newSDKClients(cfg aws.Config) *sdkClients {
	return &sdkClients{
		cfg: cfg,
		ec2: map[string]*ec2.Client{}, elb: map[string]*elb.Client{}, rds: map[string]*rds.Client{},
		s3: map[string]*s3.Client{}, iam: map[string]*iam.Client{}, sts: map[string]*sts.Client{},
	}
}

func cached[C any](mu *sync.Mutex, m map[string]C, region string, create func() C) C {
	mu.Lock()
	defer mu.Unlock()
	if c, ok := m[region]; ok {
		return c
	}
	c := create()
	m[region] = c
	return c
}

func (f *sdkClients) EC2(region string) ec2API {
	return cached(&f.mu, f.ec2, region, func() *ec2.Client {
		return ec2.NewFromConfig(f.cfg, func(o *ec2.Options) { o.Region = region })
	})
}

func (f *sdkClients) ELB(region string) elbAPI {
	return cached(&f.mu, f.elb, region, func() *elb.Client {
		return elb.NewFromConfig(f.cfg, func(o *elb.Options) { o.Region = region })
	})
}

func (f *sdkClients) RDS(region string) rdsAPI {
	return cached(&f.mu, f.rds, region, func() *rds.Client {
		return rds.NewFromConfig(f.cfg, func(o *rds.Options) { o.Region = region })
	})
}

func (f *sdkClients) S3(region string) s3API {
	return cached(&f.mu, f.s3, region, func() *s3.Client {
		return s3.NewFromConfig(f.cfg, func(o *s3.Options) { o.Region = region })
	})
}

func (f *sdkClients) IAM(region string) iamAPI {
	return cached(&f.mu, f.iam, region, func() *iam.Client {
		return iam.NewFromConfig(f.cfg, func(o *iam.Options) { o.Region = region })
	})
}

func (f *sdkClients) STS(region string) stsAPI {
	return cached(&f.mu, f.sts, region, func() *sts.Client {
		return sts.NewFromConfig(f.cfg, func(o *sts.Options) { o.Region = region })
	})
}
