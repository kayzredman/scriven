package awscloud

import (
	"context"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

// compute captures Elastic IPs, NAT gateways and EC2 instances. Instances an
// Auto Scaling group launched are left out: the group replaces them at will.
func (c *Client) compute(ctx context.Context, inv *Inventory) error {
	e := ec2.NewFromConfig(c.cfg)
	addrs, err := e.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return err
	}
	for _, a := range addrs.Addresses {
		id := aws.ToString(a.AllocationId)
		if id == "" {
			continue // EC2-Classic address
		}
		it := Item{Type: "aws_eip", ID: id, Name: nameTag(a.Tags, id), Refs: []Ref{{Value: id, Attr: "id"}}}
		if ip := aws.ToString(a.PublicIp); ip != "" {
			it.AltIDs = []string{ip}
		}
		inv.Items = append(inv.Items, it)
	}

	np := ec2.NewDescribeNatGatewaysPaginator(e, &ec2.DescribeNatGatewaysInput{})
	for np.HasMorePages() {
		page, err := np.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, g := range page.NatGateways {
			if g.State == ec2types.NatGatewayStateDeleted || g.State == ec2types.NatGatewayStateDeleting ||
				g.State == ec2types.NatGatewayStateFailed {
				continue
			}
			id := aws.ToString(g.NatGatewayId)
			inv.add("aws_nat_gateway", id, nameTag(g.Tags, id), Ref{Value: id, Attr: "id"})
		}
	}

	ip := ec2.NewDescribeInstancesPaginator(e, &ec2.DescribeInstancesInput{})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, r := range page.Reservations {
			for _, in := range r.Instances {
				id := aws.ToString(in.InstanceId)
				if in.State != nil && (in.State.Name == ec2types.InstanceStateNameTerminated ||
					in.State.Name == ec2types.InstanceStateNameShuttingDown) {
					continue
				}
				if g := tagValue(in.Tags, "aws:autoscaling:groupName"); g != "" {
					inv.skip("aws_instance", id, "launched by Auto Scaling group "+g)
					continue
				}
				inv.add("aws_instance", id, nameTag(in.Tags, id), Ref{Value: id, Attr: "id"})
			}
		}
	}
	return nil
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// loadBalancers captures application, network and gateway load balancers,
// their listeners and target groups. Listener rules beyond the default
// action are not covered yet.
func (c *Client) loadBalancers(ctx context.Context, inv *Inventory) error {
	l := elb.NewFromConfig(c.cfg)
	lp := elb.NewDescribeLoadBalancersPaginator(l, &elb.DescribeLoadBalancersInput{})
	for lp.HasMorePages() {
		page, err := lp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, lb := range page.LoadBalancers {
			arn, name := aws.ToString(lb.LoadBalancerArn), aws.ToString(lb.LoadBalancerName)
			inv.add("aws_lb", arn, name, Ref{Value: arn, Attr: "arn"})
			sp := elb.NewDescribeListenersPaginator(l, &elb.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn})
			for sp.HasMorePages() {
				ls, err := sp.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, s := range ls.Listeners {
					sa := aws.ToString(s.ListenerArn)
					inv.addOwned(arn, "aws_lb_listener", sa, name+"_"+strings.ToLower(string(s.Protocol))+"_"+strconv.Itoa(int(aws.ToInt32(s.Port))),
						Ref{Value: sa, Attr: "arn"})
				}
			}
		}
	}
	tp := elb.NewDescribeTargetGroupsPaginator(l, &elb.DescribeTargetGroupsInput{})
	for tp.HasMorePages() {
		page, err := tp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, tg := range page.TargetGroups {
			arn := aws.ToString(tg.TargetGroupArn)
			inv.add("aws_lb_target_group", arn, aws.ToString(tg.TargetGroupName), Ref{Value: arn, Attr: "arn"})
		}
	}
	return nil
}

// databases captures RDS DB instances and their subnet groups. Members of an
// Aurora cluster are left out until clusters are covered.
func (c *Client) databases(ctx context.Context, inv *Inventory) error {
	r := rds.NewFromConfig(c.cfg)
	gp := rds.NewDescribeDBSubnetGroupsPaginator(r, &rds.DescribeDBSubnetGroupsInput{})
	for gp.HasMorePages() {
		page, err := gp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, g := range page.DBSubnetGroups {
			name := aws.ToString(g.DBSubnetGroupName)
			if name == "default" {
				inv.skip("aws_db_subnet_group", name, "default DB subnet group, created by AWS")
				continue
			}
			inv.add("aws_db_subnet_group", name, name,
				Ref{Value: name, Attr: "name", Only: []string{"db_subnet_group_name"}},
				Ref{Value: aws.ToString(g.DBSubnetGroupArn), Attr: "arn"})
		}
	}
	ip := rds.NewDescribeDBInstancesPaginator(r, &rds.DescribeDBInstancesInput{})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, d := range page.DBInstances {
			id := aws.ToString(d.DBInstanceIdentifier)
			if cl := aws.ToString(d.DBClusterIdentifier); cl != "" {
				inv.skip("aws_db_instance", id, "member of Aurora cluster "+cl+" (clusters are not covered yet)")
				continue
			}
			inv.add("aws_db_instance", id, id, Ref{Value: aws.ToString(d.DBInstanceArn), Attr: "arn"})
		}
	}
	return nil
}

// functions captures Lambda functions' configuration. Their code is not
// captured: see the clean-up step, which points each at a file to supply.
func (c *Client) functions(ctx context.Context, inv *Inventory) error {
	p := lambda.NewListFunctionsPaginator(lambda.NewFromConfig(c.cfg), &lambda.ListFunctionsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, f := range page.Functions {
			name := aws.ToString(f.FunctionName)
			inv.add("aws_lambda_function", name, name,
				Ref{Value: aws.ToString(f.FunctionArn), Attr: "arn"},
				Ref{Value: name, Attr: "function_name", Only: []string{"function_name"}})
		}
	}
	return nil
}

// keys captures customer-managed KMS keys and their aliases. AWS-managed
// keys (aws/s3 and the like) belong to AWS.
func (c *Client) keys(ctx context.Context, inv *Inventory) error {
	k := kms.NewFromConfig(c.cfg)
	ours := map[string]bool{}
	kp := kms.NewListKeysPaginator(k, &kms.ListKeysInput{})
	for kp.HasMorePages() {
		page, err := kp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, key := range page.Keys {
			d, err := k.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: key.KeyId})
			if err != nil {
				if denied(err) {
					continue
				}
				return err
			}
			m := d.KeyMetadata
			id := aws.ToString(m.KeyId)
			switch {
			case m.KeyManager == kmstypes.KeyManagerTypeAws:
				continue
			case m.KeyState == kmstypes.KeyStatePendingDeletion || m.KeyState == kmstypes.KeyStatePendingReplicaDeletion:
				inv.skip("aws_kms_key", id, "scheduled for deletion")
				continue
			}
			ours[id] = true
			inv.add("aws_kms_key", id, id, Ref{Value: id, Attr: "key_id"}, Ref{Value: aws.ToString(m.Arn), Attr: "arn"})
		}
	}
	ap := kms.NewListAliasesPaginator(k, &kms.ListAliasesInput{})
	for ap.HasMorePages() {
		page, err := ap.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, a := range page.Aliases {
			name, target := aws.ToString(a.AliasName), aws.ToString(a.TargetKeyId)
			if !ours[target] {
				continue // AWS's own aliases, or an alias of a key not captured
			}
			inv.addOwned(target, "aws_kms_alias", name, strings.TrimPrefix(name, "alias/"))
		}
	}
	// A key's address uses its alias when it has one: kms_key.shop_orders
	// reads better than a UUID.
	alias := map[string]string{}
	for _, it := range inv.Items {
		if it.Type == "aws_kms_alias" && alias[it.Owner] == "" {
			alias[it.Owner] = it.Name
		}
	}
	for i, it := range inv.Items {
		if it.Type == "aws_kms_key" && alias[it.ID] != "" {
			inv.Items[i].Name = alias[it.ID]
		}
	}
	return nil
}

// stackResources maps the physical ID of every resource a live
// CloudFormation stack manages to the stack's name.
func (c *Client) stackResources(ctx context.Context) (map[string]string, error) {
	cf := cloudformation.NewFromConfig(c.cfg)
	out := map[string]string{}
	sp := cloudformation.NewListStacksPaginator(cf, &cloudformation.ListStacksInput{})
	for sp.HasMorePages() {
		page, err := sp.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.StackSummaries {
			if s.StackStatus == cftypes.StackStatusDeleteComplete {
				continue
			}
			name := aws.ToString(s.StackName)
			rp := cloudformation.NewListStackResourcesPaginator(cf, &cloudformation.ListStackResourcesInput{StackName: s.StackId})
			for rp.HasMorePages() {
				rs, err := rp.NextPage(ctx)
				if err != nil {
					return nil, err
				}
				for _, r := range rs.StackResourceSummaries {
					if id := aws.ToString(r.PhysicalResourceId); id != "" && r.ResourceStatus != cftypes.ResourceStatusDeleteComplete {
						out[id] = name
						out[strings.ToLower(id)] = name
					}
				}
			}
		}
	}
	return out, nil
}
