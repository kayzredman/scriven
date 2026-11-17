package awscloud

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// Covered lists the Terraform resource types an AWS capture includes.
// Everything else in the account is left alone and named in catalog.yaml.
var Covered = []string{
	"aws_vpc", "aws_subnet", "aws_internet_gateway", "aws_route_table", "aws_route_table_association",
	"aws_security_group", "aws_s3_bucket", "aws_s3_bucket_versioning",
	"aws_iam_role", "aws_iam_role_policy", "aws_iam_policy", "aws_iam_role_policy_attachment",
	"aws_dynamodb_table", "aws_sns_topic",
}

// Item is one resource to capture.
type Item struct {
	Type string // Terraform resource type
	ID   string // the ID `terraform import` takes
	Name string // readable name, used for the Terraform address
	Refs []Ref  // values other resources use to point at this one
}

// Ref is a value that identifies an item. Where it appears in another
// resource's code, scriven writes a reference instead of the literal, so the
// code still works when replayed into an account where the IDs differ.
type Ref struct {
	Value string
	Attr  string // attribute of the item that yields Value: id, arn, name
	// Attribute names in which Value may be replaced. Empty means anywhere:
	// only for values that cannot mean anything else (vpc-..., ARNs).
	Only []string
}

type Skipped struct {
	Kind, Name, Reason string
}

type Inventory struct {
	Items      []Item
	Skipped    []Skipped
	Unreadable []string // services the identity may not list
}

// Discover lists what the identity can see. A service it may not list is
// recorded and skipped; any other failure stops the capture.
func (c *Client) Discover(ctx context.Context) (*Inventory, error) {
	inv := &Inventory{}
	steps := []struct {
		service string
		fn      func(context.Context, *Inventory) error
	}{
		{"ec2", c.network}, {"s3", c.buckets}, {"iam", c.identity}, {"dynamodb", c.tables}, {"sns", c.topics},
	}
	for _, s := range steps {
		if err := s.fn(ctx, inv); err != nil {
			if denied(err) {
				inv.Unreadable = append(inv.Unreadable, s.service)
				continue
			}
			return nil, fmt.Errorf("listing %s: %w", s.service, err)
		}
	}
	sort.SliceStable(inv.Items, func(i, j int) bool {
		a, b := inv.Items[i], inv.Items[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})
	return inv, nil
}

func nameTag(tags []ec2types.Tag, fallback string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" && aws.ToString(t.Value) != "" {
			return aws.ToString(t.Value)
		}
	}
	return fallback
}

func (inv *Inventory) add(typ, id, name string, refs ...Ref) {
	inv.Items = append(inv.Items, Item{Type: typ, ID: id, Name: name, Refs: refs})
}

func (inv *Inventory) skip(kind, name, reason string) {
	inv.Skipped = append(inv.Skipped, Skipped{kind, name, reason})
}

// network captures VPCs and what hangs off them. AWS creates a default VPC,
// and a main route table and default security group with every VPC; those are
// left out because Terraform cannot create them, only adopt them.
func (c *Client) network(ctx context.Context, inv *Inventory) error {
	e := ec2.NewFromConfig(c.cfg)
	defaultVPC := map[string]bool{}
	names := map[string]string{}

	vp := ec2.NewDescribeVpcsPaginator(e, &ec2.DescribeVpcsInput{})
	for vp.HasMorePages() {
		page, err := vp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, v := range page.Vpcs {
			id := aws.ToString(v.VpcId)
			names[id] = nameTag(v.Tags, id)
			if aws.ToBool(v.IsDefault) {
				defaultVPC[id] = true
				inv.skip("aws_vpc", id, "default VPC, created by AWS")
				continue
			}
			inv.add("aws_vpc", id, names[id], Ref{Value: id, Attr: "id"})
		}
	}

	sp := ec2.NewDescribeSubnetsPaginator(e, &ec2.DescribeSubnetsInput{})
	for sp.HasMorePages() {
		page, err := sp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, s := range page.Subnets {
			id := aws.ToString(s.SubnetId)
			names[id] = nameTag(s.Tags, id)
			if defaultVPC[aws.ToString(s.VpcId)] {
				continue // part of the default VPC, already recorded
			}
			inv.add("aws_subnet", id, names[id], Ref{Value: id, Attr: "id"})
		}
	}

	ip := ec2.NewDescribeInternetGatewaysPaginator(e, &ec2.DescribeInternetGatewaysInput{})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, g := range page.InternetGateways {
			id := aws.ToString(g.InternetGatewayId)
			if len(g.Attachments) > 0 && defaultVPC[aws.ToString(g.Attachments[0].VpcId)] {
				continue
			}
			inv.add("aws_internet_gateway", id, nameTag(g.Tags, id), Ref{Value: id, Attr: "id"})
		}
	}

	rp := ec2.NewDescribeRouteTablesPaginator(e, &ec2.DescribeRouteTablesInput{})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, rt := range page.RouteTables {
			id := aws.ToString(rt.RouteTableId)
			if defaultVPC[aws.ToString(rt.VpcId)] {
				continue
			}
			main := false
			for _, a := range rt.Associations {
				main = main || aws.ToBool(a.Main)
			}
			if main {
				inv.skip("aws_route_table", id, "main route table of "+names[aws.ToString(rt.VpcId)]+", created with the VPC")
				continue
			}
			name := nameTag(rt.Tags, id)
			inv.add("aws_route_table", id, name, Ref{Value: id, Attr: "id"})
			for _, a := range rt.Associations {
				if sub := aws.ToString(a.SubnetId); sub != "" {
					inv.add("aws_route_table_association", sub+"/"+id, names[sub]+"_"+name)
				}
			}
		}
	}

	gp := ec2.NewDescribeSecurityGroupsPaginator(e, &ec2.DescribeSecurityGroupsInput{})
	for gp.HasMorePages() {
		page, err := gp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, g := range page.SecurityGroups {
			id, name := aws.ToString(g.GroupId), aws.ToString(g.GroupName)
			if defaultVPC[aws.ToString(g.VpcId)] {
				continue
			}
			if name == "default" {
				inv.skip("aws_security_group", id, "default security group of "+names[aws.ToString(g.VpcId)]+", created with the VPC")
				continue
			}
			inv.add("aws_security_group", id, name, Ref{Value: id, Attr: "id"})
		}
	}
	return nil
}

// buckets captures S3 buckets in this target's region. Bucket names are
// global, so a bucket elsewhere belongs to that region's target.
func (c *Client) buckets(ctx context.Context, inv *Inventory) error {
	s := s3.NewFromConfig(c.cfg, func(o *s3.Options) { o.UsePathStyle = c.Endpoint != "" })
	out, err := s.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return err
	}
	for _, b := range out.Buckets {
		name := aws.ToString(b.Name)
		loc, err := s.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: b.Name})
		if err != nil {
			if denied(err) {
				inv.skip("aws_s3_bucket", name, "the identity may not read its location")
				continue
			}
			return err
		}
		region := string(loc.LocationConstraint)
		if region == "" {
			region = "us-east-1" // how S3 spells its original region
		}
		if region != c.Region {
			inv.skip("aws_s3_bucket", name, "in "+region+", not this target's region")
			continue
		}
		inv.add("aws_s3_bucket", name, name, Ref{Value: name, Attr: "id", Only: []string{"bucket"}},
			Ref{Value: "arn:" + c.partition() + ":s3:::" + name, Attr: "arn"})
		v, err := s.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b.Name})
		if err != nil {
			if denied(err) {
				continue
			}
			return err
		}
		if v.Status != "" {
			inv.add("aws_s3_bucket_versioning", name, name)
		}
	}
	return nil
}

// identity captures IAM roles and customer-managed policies. IAM is global:
// an account with targets in several regions gets them in each.
func (c *Client) identity(ctx context.Context, inv *Inventory) error {
	i := iam.NewFromConfig(c.cfg)
	pp := iam.NewListPoliciesPaginator(i, &iam.ListPoliciesInput{Scope: iamtypes.PolicyScopeTypeLocal})
	for pp.HasMorePages() {
		page, err := pp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, p := range page.Policies {
			arn := aws.ToString(p.Arn)
			inv.add("aws_iam_policy", arn, aws.ToString(p.PolicyName), Ref{Value: arn, Attr: "arn"})
		}
	}
	rp := iam.NewListRolesPaginator(i, &iam.ListRolesInput{})
	for rp.HasMorePages() {
		page, err := rp.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, r := range page.Roles {
			name, path := aws.ToString(r.RoleName), aws.ToString(r.Path)
			if strings.HasPrefix(path, "/aws-service-role/") || strings.HasPrefix(path, "/aws-reserved/") {
				inv.skip("aws_iam_role", name, "service-linked role, managed by AWS")
				continue
			}
			inv.add("aws_iam_role", name, name,
				Ref{Value: name, Attr: "name", Only: []string{"role"}}, Ref{Value: aws.ToString(r.Arn), Attr: "arn"})

			ap := iam.NewListAttachedRolePoliciesPaginator(i, &iam.ListAttachedRolePoliciesInput{RoleName: r.RoleName})
			for ap.HasMorePages() {
				ps, err := ap.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, a := range ps.AttachedPolicies {
					inv.add("aws_iam_role_policy_attachment", name+"/"+aws.ToString(a.PolicyArn), name+"_"+aws.ToString(a.PolicyName))
				}
			}
			ip := iam.NewListRolePoliciesPaginator(i, &iam.ListRolePoliciesInput{RoleName: r.RoleName})
			for ip.HasMorePages() {
				ps, err := ip.NextPage(ctx)
				if err != nil {
					return err
				}
				for _, pn := range ps.PolicyNames {
					inv.add("aws_iam_role_policy", name+":"+pn, name+"_"+pn)
				}
			}
		}
	}
	return nil
}

func (c *Client) tables(ctx context.Context, inv *Inventory) error {
	p := dynamodb.NewListTablesPaginator(dynamodb.NewFromConfig(c.cfg), &dynamodb.ListTablesInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, t := range page.TableNames {
			inv.add("aws_dynamodb_table", t, t,
				Ref{Value: fmt.Sprintf("arn:%s:dynamodb:%s:%s:table/%s", c.partition(), c.Region, c.Account, t), Attr: "arn"})
		}
	}
	return nil
}

func (c *Client) topics(ctx context.Context, inv *Inventory) error {
	p := sns.NewListTopicsPaginator(sns.NewFromConfig(c.cfg), &sns.ListTopicsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, t := range page.Topics {
			arn := aws.ToString(t.TopicArn)
			inv.add("aws_sns_topic", arn, arn[strings.LastIndex(arn, ":")+1:], Ref{Value: arn, Attr: "arn"})
		}
	}
	return nil
}
