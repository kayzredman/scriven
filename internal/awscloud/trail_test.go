package awscloud

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
)

func TestParseEvent(t *testing.T) {
	raw := `{"userIdentity":{"type":"IAMUser","arn":"arn:aws:iam::111122223333:user/alice"},
	  "userAgent":"AWS Internal","requestParameters":{"groupId":"sg-0abc123","ipPermissions":{"items":[{"fromPort":22}]}},
	  "responseElements":{"_return":true}}`
	ev, ok := parseEvent(cttypes.Event{EventId: aws.String("e1"), EventName: aws.String("AuthorizeSecurityGroupIngress"),
		EventSource: aws.String("ec2.amazonaws.com"), EventTime: aws.Time(time.Now()), CloudTrailEvent: aws.String(raw)})
	if !ok || ev.Who != "user/alice" || ev.Agent != "console" || !ev.Touches("sg-0abc123") {
		t.Errorf("got ok=%v %+v", ok, ev)
	}
	failed := `{"userIdentity":{"arn":"arn:aws:iam::1:user/bob"},"errorCode":"AccessDenied"}`
	if _, ok := parseEvent(cttypes.Event{EventSource: aws.String("ec2.amazonaws.com"), CloudTrailEvent: aws.String(failed)}); ok {
		t.Error("a failed call changed nothing and should be dropped")
	}
	if _, ok := parseEvent(cttypes.Event{EventSource: aws.String("sqs.amazonaws.com"), CloudTrailEvent: aws.String(raw)}); ok {
		t.Error("a service scriven does not capture should be dropped")
	}
}

func TestTouches(t *testing.T) {
	ev := func(vals ...string) WriteEvent { return WriteEvent{Values: vals} }
	cases := []struct {
		ev   WriteEvent
		id   string
		want bool
	}{
		{ev("shop-app"), "shop-app/arn:aws:iam::1:policy/read", true},      // a role's attached policy
		{ev("shop-app"), "shop-app:orders-table", true},                    // a role's inline policy
		{ev("rtb-0123"), "subnet-9/rtb-0123", true},                        // an association, by its route table
		{ev("arn:aws:kms:eu-west-1:1:key/abcd-1234"), "abcd-1234", true},   // a key named by ARN
		{ev("arn:aws:lambda:eu-west-1:1:function:export"), "export", true}, // a function named by ARN
		{ev("shop-assets"), "shop-assets-lab", false},                      // a prefix is not a match
		{ev("sg-0abc123"), "sg-0abc1234", false},
	}
	for _, c := range cases {
		if got := c.ev.Touches(c.id); got != c.want {
			t.Errorf("%v touches %q = %v, want %v", c.ev.Values, c.id, got, c.want)
		}
	}
}

func TestAgentAndIdentity(t *testing.T) {
	for ua, want := range map[string]string{
		"APN/1.0 HashiCorp/1.0 Terraform/1.17.1 (+https://www.terraform.io) terraform-provider-aws/6.73.0": "Terraform",
		"aws-cli/2.31.0 md/awscrt#0.27": "aws-cli",
		"console.amazonaws.com":         "console",
		"cloudformation.amazonaws.com":  "CloudFormation",
	} {
		if got := agentOf(ua); got != want {
			t.Errorf("agentOf(%q) = %q, want %q", ua, got, want)
		}
	}
	if got := shortIdentity("arn:aws:sts::1:assumed-role/admin/bob", "AssumedRole", "", ""); got != "assumed-role/admin/bob" {
		t.Errorf("assumed role: %q", got)
	}
	if got := shortIdentity("", "AWSService", "autoscaling.amazonaws.com", ""); got != "autoscaling.amazonaws.com" {
		t.Errorf("service: %q", got)
	}
}
