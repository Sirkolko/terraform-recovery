package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

func ec2Tags(tags []ec2types.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return out
}

func collectVPCs(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeVpcsPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeVpcsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, v := range page.Vpcs {
			r := newResource("aws_vpc", sc.region, aws.ToString(v.VpcId), sc.account)
			r.Tags = ec2Tags(v.Tags)
			withName(r, "")
			r.ARN = sc.arn("ec2", sc.region, "vpc/"+r.ID)
			r.Identifiers["arn"] = r.ARN
			setAttr(r, "cidr_block", aws.ToString(v.CidrBlock))
			setAttr(r, "instance_tenancy", string(v.InstanceTenancy))
			if aws.ToBool(v.IsDefault) {
				setAttr(r, "is_default", "true")
				r.AutoIgnore = "Default VPC created by AWS"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectSubnets(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeSubnetsPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeSubnetsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, s := range page.Subnets {
			r := newResource("aws_subnet", sc.region, aws.ToString(s.SubnetId), sc.account)
			r.Tags = ec2Tags(s.Tags)
			withName(r, "")
			r.ARN = aws.ToString(s.SubnetArn)
			r.Identifiers["arn"] = r.ARN
			setAttr(r, "cidr_block", aws.ToString(s.CidrBlock))
			setAttr(r, "availability_zone", aws.ToString(s.AvailabilityZone))
			setAttr(r, "availability_zone_id", aws.ToString(s.AvailabilityZoneId))
			setBool(r, "map_public_ip_on_launch", s.MapPublicIpOnLaunch)
			for _, a := range s.Ipv6CidrBlockAssociationSet {
				setAttr(r, "ipv6_cidr_block", aws.ToString(a.Ipv6CidrBlock))
			}
			setRel(r, "vpc_id", aws.ToString(s.VpcId))
			if aws.ToBool(s.DefaultForAz) {
				setAttr(r, "default_for_az", "true")
				r.AutoIgnore = "Default subnet of the default VPC"
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectRouteTables(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeRouteTablesPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeRouteTablesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, rt := range page.RouteTables {
			r := newResource("aws_route_table", sc.region, aws.ToString(rt.RouteTableId), sc.account)
			r.Tags = ec2Tags(rt.Tags)
			withName(r, "")
			r.ARN = sc.arn("ec2", sc.region, "route-table/"+r.ID)
			r.Identifiers["arn"] = r.ARN
			setRel(r, "vpc_id", aws.ToString(rt.VpcId))
			for _, route := range rt.Routes {
				// Skip the implicit local route and propagated routes, which
				// are not configured in Terraform.
				if route.Origin != ec2types.RouteOriginCreateRoute {
					continue
				}
				setAttr(r, "route.cidr_block", aws.ToString(route.DestinationCidrBlock))
				setAttr(r, "route.ipv6_cidr_block", aws.ToString(route.DestinationIpv6CidrBlock))
				setAttr(r, "route.destination", aws.ToString(route.DestinationCidrBlock),
					aws.ToString(route.DestinationIpv6CidrBlock), aws.ToString(route.DestinationPrefixListId))
				if gw := aws.ToString(route.GatewayId); gw != "local" {
					setRel(r, "route.gateway_id", gw)
				}
				setRel(r, "route.nat_gateway_id", aws.ToString(route.NatGatewayId))
				setRel(r, "route.transit_gateway_id", aws.ToString(route.TransitGatewayId))
				setRel(r, "route.vpc_peering_connection_id", aws.ToString(route.VpcPeeringConnectionId))
				setRel(r, "route.network_interface_id", aws.ToString(route.NetworkInterfaceId))
			}
			for _, a := range rt.Associations {
				if aws.ToBool(a.Main) {
					setAttr(r, "main", "true")
				}
				setRel(r, "association.subnet_id", aws.ToString(a.SubnetId))
				setRel(r, "association.gateway_id", aws.ToString(a.GatewayId))
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectInternetGateways(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeInternetGatewaysPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeInternetGatewaysInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, g := range page.InternetGateways {
			r := newResource("aws_internet_gateway", sc.region, aws.ToString(g.InternetGatewayId), sc.account)
			r.Tags = ec2Tags(g.Tags)
			withName(r, "")
			r.ARN = sc.arn("ec2", sc.region, "internet-gateway/"+r.ID)
			r.Identifiers["arn"] = r.ARN
			for _, a := range g.Attachments {
				setRel(r, "vpc_id", aws.ToString(a.VpcId))
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectNatGateways(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeNatGatewaysPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeNatGatewaysInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, n := range page.NatGateways {
			if n.State != ec2types.NatGatewayStateAvailable && n.State != ec2types.NatGatewayStatePending {
				continue // deleted gateways remain visible for a while
			}
			r := newResource("aws_nat_gateway", sc.region, aws.ToString(n.NatGatewayId), sc.account)
			r.Tags = ec2Tags(n.Tags)
			withName(r, "")
			r.ARN = sc.arn("ec2", sc.region, "natgateway/"+r.ID)
			r.Identifiers["arn"] = r.ARN
			setAttr(r, "connectivity_type", string(n.ConnectivityType))
			setRel(r, "subnet_id", aws.ToString(n.SubnetId))
			setRel(r, "vpc_id", aws.ToString(n.VpcId))
			for _, a := range n.NatGatewayAddresses {
				setRel(r, "allocation_id", aws.ToString(a.AllocationId))
				setAttr(r, "public_ip", aws.ToString(a.PublicIp))
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectSecurityGroups(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeSecurityGroupsPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeSecurityGroupsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, g := range page.SecurityGroups {
			r := newResource("aws_security_group", sc.region, aws.ToString(g.GroupId), sc.account)
			r.Tags = ec2Tags(g.Tags)
			withName(r, aws.ToString(g.GroupName))
			r.ARN = aws.ToString(g.SecurityGroupArn)
			if r.ARN == "" {
				r.ARN = sc.arn("ec2", sc.region, "security-group/"+r.ID)
			}
			r.Identifiers["arn"] = r.ARN
			r.Identifiers["name"] = aws.ToString(g.GroupName)
			setAttr(r, "name", aws.ToString(g.GroupName))
			setAttr(r, "description", aws.ToString(g.Description))
			setRel(r, "vpc_id", aws.ToString(g.VpcId))
			for _, perm := range g.IpPermissions {
				for _, pair := range perm.UserIdGroupPairs {
					setRel(r, "ingress.security_groups", aws.ToString(pair.GroupId))
				}
			}
			for _, perm := range g.IpPermissionsEgress {
				for _, pair := range perm.UserIdGroupPairs {
					setRel(r, "egress.security_groups", aws.ToString(pair.GroupId))
				}
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectSecurityGroupRules(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeSecurityGroupRulesPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeSecurityGroupRulesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, rule := range page.SecurityGroupRules {
			r := newResource("aws_vpc_security_group_rule", sc.region, aws.ToString(rule.SecurityGroupRuleId), sc.account)
			r.Tags = ec2Tags(rule.Tags)
			r.ARN = aws.ToString(rule.SecurityGroupRuleArn)
			if r.ARN != "" {
				r.Identifiers["arn"] = r.ARN
			}
			direction := "ingress"
			if aws.ToBool(rule.IsEgress) {
				direction = "egress"
			}
			setAttr(r, "direction", direction)
			setAttr(r, "ip_protocol", aws.ToString(rule.IpProtocol))
			if aws.ToString(rule.IpProtocol) != "-1" {
				setInt(r, "from_port", rule.FromPort)
				setInt(r, "to_port", rule.ToPort)
			}
			setAttr(r, "cidr_ipv4", aws.ToString(rule.CidrIpv4))
			setAttr(r, "cidr_ipv6", aws.ToString(rule.CidrIpv6))
			setAttr(r, "prefix_list_id", aws.ToString(rule.PrefixListId))
			setAttr(r, "description", aws.ToString(rule.Description))
			setRel(r, "security_group_id", aws.ToString(rule.GroupId))
			if rule.ReferencedGroupInfo != nil {
				setRel(r, "referenced_security_group_id", aws.ToString(rule.ReferencedGroupInfo.GroupId))
			}
			source := aws.ToString(rule.CidrIpv4) + aws.ToString(rule.CidrIpv6) + aws.ToString(rule.PrefixListId)
			if rule.ReferencedGroupInfo != nil {
				source += aws.ToString(rule.ReferencedGroupInfo.GroupId)
			}
			ports := "all"
			if aws.ToString(rule.IpProtocol) != "-1" {
				ports = fmt.Sprintf("%s %d-%d", aws.ToString(rule.IpProtocol), aws.ToInt32(rule.FromPort), aws.ToInt32(rule.ToPort))
			}
			withName(r, fmt.Sprintf("%s %s %s %s", aws.ToString(rule.GroupId), direction, ports, source))
			out = append(out, r)
		}
	}
	return out, nil
}

func collectInstances(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeInstancesPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeInstancesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, res := range page.Reservations {
			for _, in := range res.Instances {
				state := ""
				if in.State != nil {
					state = string(in.State.Name)
				}
				if state == string(ec2types.InstanceStateNameTerminated) || state == string(ec2types.InstanceStateNameShuttingDown) {
					continue
				}
				r := newResource("aws_instance", sc.region, aws.ToString(in.InstanceId), sc.account)
				r.Tags = ec2Tags(in.Tags)
				withName(r, "")
				r.ARN = sc.arn("ec2", sc.region, "instance/"+r.ID)
				r.Identifiers["arn"] = r.ARN
				setAttr(r, "instance_type", string(in.InstanceType))
				setAttr(r, "ami", aws.ToString(in.ImageId))
				if in.Placement != nil {
					setAttr(r, "availability_zone", aws.ToString(in.Placement.AvailabilityZone))
				}
				setAttr(r, "key_name", aws.ToString(in.KeyName))
				setAttr(r, "private_ip", aws.ToString(in.PrivateIpAddress))
				setAttr(r, "instance_state", state)
				setAttr(r, "root_device_name", aws.ToString(in.RootDeviceName))
				setRel(r, "subnet_id", aws.ToString(in.SubnetId))
				setRel(r, "vpc_id", aws.ToString(in.VpcId))
				for _, g := range in.SecurityGroups {
					setRel(r, "vpc_security_group_ids", aws.ToString(g.GroupId))
				}
				if in.IamInstanceProfile != nil {
					arn := aws.ToString(in.IamInstanceProfile.Arn)
					setRel(r, "iam_instance_profile", arn, arn[strings.LastIndex(arn, "/")+1:])
				}
				for _, bd := range in.BlockDeviceMappings {
					if bd.Ebs != nil {
						setRel(r, "attached_volume_ids", aws.ToString(bd.Ebs.VolumeId))
					}
				}
				if state == string(ec2types.InstanceStateNameStopped) {
					r.Hints = append(r.Hints, "instance is stopped")
				}
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func collectVolumes(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	var out []*models.CloudResource
	p := ec2.NewDescribeVolumesPaginator(sc.s.clients.EC2(sc.region), &ec2.DescribeVolumesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, v := range page.Volumes {
			if v.State == ec2types.VolumeStateDeleting || v.State == ec2types.VolumeStateDeleted {
				continue
			}
			r := newResource("aws_ebs_volume", sc.region, aws.ToString(v.VolumeId), sc.account)
			r.Tags = ec2Tags(v.Tags)
			withName(r, "")
			r.ARN = sc.arn("ec2", sc.region, "volume/"+r.ID)
			r.Identifiers["arn"] = r.ARN
			setAttr(r, "availability_zone", aws.ToString(v.AvailabilityZone))
			setInt(r, "size", v.Size)
			setAttr(r, "type", string(v.VolumeType))
			setInt(r, "iops", v.Iops)
			setInt(r, "throughput", v.Throughput)
			setBool(r, "encrypted", v.Encrypted)
			setAttr(r, "snapshot_id", aws.ToString(v.SnapshotId))
			setAttr(r, "kms_key_id", aws.ToString(v.KmsKeyId))
			for _, a := range v.Attachments {
				setRel(r, "attachment.instance_id", aws.ToString(a.InstanceId))
				setAttr(r, "attachment.device", aws.ToString(a.Device))
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func collectAddresses(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	page, err := sc.s.clients.EC2(sc.region).DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, err
	}
	var out []*models.CloudResource
	for _, a := range page.Addresses {
		if aws.ToString(a.AllocationId) == "" {
			continue
		}
		r := newResource("aws_eip", sc.region, aws.ToString(a.AllocationId), sc.account)
		r.Tags = ec2Tags(a.Tags)
		withName(r, aws.ToString(a.PublicIp))
		r.ARN = sc.arn("ec2", sc.region, "elastic-ip/"+r.ID)
		r.Identifiers["arn"] = r.ARN
		r.Identifiers["allocation_id"] = r.ID
		setAttr(r, "public_ip", aws.ToString(a.PublicIp))
		setAttr(r, "address", aws.ToString(a.PublicIp))
		setAttr(r, "domain", string(a.Domain))
		setAttr(r, "association_id", aws.ToString(a.AssociationId))
		setAttr(r, "private_ip", aws.ToString(a.PrivateIpAddress))
		setRel(r, "instance", aws.ToString(a.InstanceId))
		setRel(r, "network_interface", aws.ToString(a.NetworkInterfaceId))
		out = append(out, r)
	}
	return out, nil
}

func collectKeyPairs(ctx context.Context, sc *scanContext) ([]*models.CloudResource, error) {
	page, err := sc.s.clients.EC2(sc.region).DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{})
	if err != nil {
		return nil, err
	}
	var out []*models.CloudResource
	for _, k := range page.KeyPairs {
		r := newResource("aws_key_pair", sc.region, aws.ToString(k.KeyName), sc.account)
		r.Tags = ec2Tags(k.Tags)
		withName(r, r.ID)
		r.ARN = sc.arn("ec2", sc.region, "key-pair/"+r.ID)
		r.Identifiers["arn"] = r.ARN
		r.Identifiers["key_name"] = r.ID
		r.Identifiers["key_pair_id"] = aws.ToString(k.KeyPairId)
		setAttr(r, "key_name", r.ID)
		setAttr(r, "key_type", string(k.KeyType))
		out = append(out, r)
	}
	return out, nil
}
