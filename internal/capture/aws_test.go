package capture

import (
	"strings"
	"testing"

	"scriven/internal/awscloud"
)

const awsGenerated = `# __generated__ by Terraform
resource "aws_vpc" "shop" {
  cidr_block          = "10.20.0.0/16"
  enable_dns_support  = true
  ipv6_netmask_length = 0
  region              = "eu-west-1"
  tags = {
    Name = "shop"
  }
  tags_all = {
    Name = "shop"
  }
}

# __generated__ by Terraform
resource "aws_subnet" "shop_a" {
  availability_zone               = "eu-west-1a"
  availability_zone_id            = "euw1-az3"
  cidr_block                      = "10.20.1.0/24"
  map_customer_owned_ip_on_launch = false
  map_public_ip_on_launch         = false
  outpost_arn                     = null
  region                          = "eu-west-1"
  vpc_id                          = "vpc-111"
}

# __generated__ by Terraform
resource "aws_route_table" "public" {
  region = "eu-west-1"
  route = [{
    cidr_block      = "0.0.0.0/0"
    gateway_id      = "igw-333"
    ipv6_cidr_block = ""
  }]
  vpc_id = "vpc-111"
}

# __generated__ by Terraform
resource "aws_security_group" "a" {
  ingress = [{
    from_port       = 0
    security_groups = ["sg-bbb"]
  }]
  name   = "a"
  vpc_id = "vpc-111"
}

# __generated__ by Terraform
resource "aws_security_group" "b" {
  ingress = [{
    from_port       = 0
    security_groups = ["sg-aaa"]
  }]
  name   = "b"
  vpc_id = "vpc-111"
}

# __generated__ by Terraform
resource "aws_iam_role" "app" {
  name = "shop-app"
  tags = {
    owner = "shop-app"
  }
}

# __generated__ by Terraform
resource "aws_iam_role_policy_attachment" "app" {
  policy_arn = "arn:aws:iam::123456789012:policy/read"
  role       = "shop-app"
}
`

func awsItems() map[string]awscloud.Item {
	id := func(v string) []awscloud.Ref { return []awscloud.Ref{{Value: v, Attr: "id"}} }
	return map[string]awscloud.Item{
		"aws_vpc.shop":           {Type: "aws_vpc", ID: "vpc-111", Refs: id("vpc-111")},
		"aws_subnet.shop_a":      {Type: "aws_subnet", ID: "subnet-222", Refs: id("subnet-222")},
		"aws_route_table.public": {Type: "aws_route_table", ID: "rtb-444", Refs: id("rtb-444")},
		"aws_security_group.a":   {Type: "aws_security_group", ID: "sg-aaa", Refs: id("sg-aaa")},
		"aws_security_group.b":   {Type: "aws_security_group", ID: "sg-bbb", Refs: id("sg-bbb")},
		"aws_iam_role.app": {Type: "aws_iam_role", ID: "shop-app",
			Refs: []awscloud.Ref{{Value: "shop-app", Attr: "name", Only: []string{"role"}}}},
		"aws_iam_role_policy_attachment.app": {Type: "aws_iam_role_policy_attachment", ID: "shop-app/arn"},
		// Discovered but not imported (no generated block): must never be referenced.
		"aws_internet_gateway.shop": {Type: "aws_internet_gateway", ID: "igw-333", Refs: id("igw-333")},
	}
}

func TestCleanAWS(t *testing.T) {
	out, st, err := cleanAWS([]byte(awsGenerated), awsItems())
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, gone := range []string{"tags_all", "region", "availability_zone_id", "map_customer_owned_ip_on_launch",
		"outpost_arn", "ipv6_netmask_length", "__generated__"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q should have been removed:\n%s", gone, got)
		}
	}
	for _, kept := range []string{
		`map_public_ip_on_launch = false`, // false is a real setting, kept
		`from_port       = 0`,             // zero inside a rule is a real port
		`ipv6_cidr_block = null`,          // "" in a route object becomes null
		`vpc_id = aws_vpc.shop.id`,
		`role       = aws_iam_role.app.name`,
		`owner = "shop-app"`,          // same text as the role name, but not a reference to it
		`gateway_id      = "igw-333"`, // its target was not imported: stays a literal
		`name = "shop-app"`,           // a resource never refers to itself
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("expected %q in:\n%s", kept, got)
		}
	}
	// a and b point at each other. One becomes a reference, the other must
	// stay a literal or Terraform would report a cycle.
	refs := strings.Count(got, "aws_security_group.a.id") + strings.Count(got, "aws_security_group.b.id")
	if refs != 1 {
		t.Errorf("want exactly one reference between the two security groups, got %d:\n%s", refs, got)
	}
	if st.linked != 6 { // vpc_id x4, role, one security group
		t.Errorf("linked = %d, want 6", st.linked)
	}
	// Same input, same bytes: the library must not churn between captures.
	again, _, _ := cleanAWS([]byte(awsGenerated), awsItems())
	if string(again) != got {
		t.Error("cleanAWS is not deterministic")
	}
}

func TestAWSAddressesAreUnique(t *testing.T) {
	got := awsAddresses([]awscloud.Item{
		{Type: "aws_vpc", ID: "vpc-1", Name: "shop"},
		{Type: "aws_vpc", ID: "vpc-2", Name: "Shop"},
		{Type: "aws_s3_bucket", ID: "9lives", Name: "9lives"},
	})
	for _, want := range []string{"aws_vpc.shop", "aws_vpc.shop_2", "aws_s3_bucket.r_9lives"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing address %s in %v", want, got)
		}
	}
}

func TestAWSNote(t *testing.T) {
	if awsNote("aws_s3_bucket.x", leafDiff{path: []string{"force_destroy"}}) == "" {
		t.Error("force_destroy is a Terraform-only setting and should be a note")
	}
	if awsNote("aws_security_group.x", leafDiff{path: []string{"ingress", "0", "from_port"}}) != "" {
		t.Error("a changed rule is real drift, not a note")
	}
}

func TestImportFailure(t *testing.T) {
	out := "aws_x.y: Refreshing...\n\nError: Cannot import non-existent remote object\n\nWhile attempting to import an existing object to\n" +
		"\"aws_s3_bucket.assets\", the provider detected that no object exists\n\nError: something else\n\n  with aws_vpc.shop,\n"
	if got := importFailure(out, "aws_s3_bucket.assets", "assets"); got != "Cannot import non-existent remote object" {
		t.Errorf("got %q", got)
	}
	if got := importFailure(out, "aws_vpc.shop", "vpc-1"); got != "something else" {
		t.Errorf("got %q", got)
	}
}
