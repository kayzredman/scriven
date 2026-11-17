package awscloud

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func TestPolicySourceARN(t *testing.T) {
	for in, want := range map[string]string{
		"arn:aws:iam::123456789012:user/scriven":                    "arn:aws:iam::123456789012:user/scriven",
		"arn:aws:iam::123456789012:role/scriven":                    "arn:aws:iam::123456789012:role/scriven",
		"arn:aws:sts::123456789012:assumed-role/scriven/session-1":  "arn:aws:iam::123456789012:role/scriven",
		"arn:aws-cn:sts::123456789012:assumed-role/scriven/session": "arn:aws-cn:iam::123456789012:role/scriven",
		"arn:aws:sts::123456789012:federated-user/bob":              "",
		"not-an-arn": "",
	} {
		if got := policySourceARN(in); got != want {
			t.Errorf("policySourceARN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDenied(t *testing.T) {
	api := func(code string) error { return &smithy.GenericAPIError{Code: code} }
	for _, code := range []string{"AccessDenied", "UnauthorizedOperation", "AccessDeniedException"} {
		if !denied(api(code)) {
			t.Errorf("%s should count as denied", code)
		}
	}
	for _, err := range []error{api("DryRunOperation"), api("InvalidVpcID.NotFound"), errors.New("timeout"), nil} {
		if denied(err) {
			t.Errorf("%v should not count as denied", err)
		}
	}
}

func TestEnv(t *testing.T) {
	c := &Client{Region: "eu-west-1", Endpoint: "http://localhost:5000",
		opt: Options{AccessKeyID: "AKIA", SecretAccessKey: "s3cr3t"}}
	env := strings.Join(c.Env(), "\n")
	for _, want := range []string{"AWS_REGION=eu-west-1", "AWS_ACCESS_KEY_ID=AKIA", "AWS_SECRET_ACCESS_KEY=s3cr3t",
		"AWS_ENDPOINT_URL=http://localhost:5000", "HTTP_PROXY=http://localhost:5000"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in:\n%s", want, env)
		}
	}
	real := &Client{Region: "eu-west-1"}
	if e := strings.Join(real.Env(), "\n"); strings.Contains(e, "PROXY") || strings.Contains(e, "ENDPOINT") || real.EmulatorOverride() != nil {
		t.Errorf("a real AWS target must get no emulator settings:\n%s", e)
	}
}

func TestLeaveOutManaged(t *testing.T) {
	inv := &Inventory{Items: []Item{
		{Type: "aws_vpc", ID: "vpc-1"},
		{Type: "aws_iam_role", ID: "shop-app"},
		{Type: "aws_iam_role_policy_attachment", ID: "shop-app/arn:aws:iam::1:policy/p", Owner: "shop-app"},
		{Type: "aws_eip", ID: "eipalloc-1", AltIDs: []string{"203.0.113.7"}},
		{Type: "aws_db_instance", ID: "Orders-DB"},
		{Type: "aws_s3_bucket", ID: "by-hand"},
	}}
	inv.leaveOutManaged(map[string]string{"vpc-1": "net", "shop-app": "app", "203.0.113.7": "net", "orders-db": "app"})
	if len(inv.Items) != 1 || inv.Items[0].ID != "by-hand" {
		t.Errorf("kept %+v, want only the bucket made by hand", inv.Items)
	}
	if inv.Stacks["net"] != 2 || inv.Stacks["app"] != 3 {
		t.Errorf("stack counts %v, want net=2 app=3", inv.Stacks)
	}
}
