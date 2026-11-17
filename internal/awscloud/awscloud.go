// Package awscloud is scriven's read-only view of one AWS account and region:
// who it is logged in as, proof that the identity cannot write, and the list
// of resources to capture.
package awscloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

type Options struct {
	Region          string
	Profile         string // shared-config profile; blank uses the default credential chain
	AccessKeyID     string // resolved secret, never a reference
	SecretAccessKey string
	Endpoint        string // emulators only (Moto, LocalStack)
	RoleARN         string // role to assume after logging in
	ExternalID      string
}

type Client struct {
	cfg      aws.Config
	opt      Options
	creds    aws.Credentials // assumed-role credentials handed to Terraform
	Region   string
	Endpoint string
	Account  string
	ARN      string
}

func Connect(ctx context.Context, o Options) (*Client, error) {
	if o.Region == "" {
		return nil, errors.New("aws: region is required")
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
	if o.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(o.Profile))
	}
	if o.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(o.AccessKeyID, o.SecretAccessKey, "")))
	}
	if o.Endpoint != "" {
		opts = append(opts, awsconfig.WithBaseEndpoint(o.Endpoint))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	var creds aws.Credentials
	if o.RoleARN != "" {
		p := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), o.RoleARN, func(a *stscreds.AssumeRoleOptions) {
			a.RoleSessionName = "scriven"
			a.Duration = time.Hour
			if o.ExternalID != "" {
				a.ExternalID = aws.String(o.ExternalID)
			}
		})
		cfg.Credentials = aws.NewCredentialsCache(p)
		if creds, err = cfg.Credentials.Retrieve(ctx); err != nil {
			return nil, fmt.Errorf("aws: cannot assume %s: %w", o.RoleARN, err)
		}
	}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("aws: cannot identify the caller in %s: %w", o.Region, err)
	}
	return &Client{cfg: cfg, opt: o, creds: creds, Region: o.Region, Endpoint: o.Endpoint,
		Account: aws.ToString(id.Account), ARN: aws.ToString(id.Arn)}, nil
}

// partition is "aws", "aws-cn" or "aws-us-gov", read from the caller's ARN.
func (c *Client) partition() string {
	if p := strings.SplitN(c.ARN, ":", 3); len(p) == 3 && p[1] != "" {
		return p[1]
	}
	return "aws"
}

// Where describes the target for catalog.yaml and messages.
func (c *Client) Where() string {
	s := fmt.Sprintf("aws account %s, %s", c.Account, c.Region)
	if c.Endpoint != "" {
		s += " (endpoint " + c.Endpoint + ")"
	}
	return s
}

// Env is what the Terraform AWS provider needs to log in as the same identity.
// Credentials are passed in the environment only, never written to a file.
func (c *Client) Env() []string {
	env := []string{"AWS_REGION=" + c.Region, "AWS_DEFAULT_REGION=" + c.Region}
	switch {
	case c.creds.AccessKeyID != "":
		// An assumed role: Terraform gets its temporary credentials (an hour,
		// longer than any capture), not the role setting, so the library's
		// provider block needs no account-specific settings.
		env = append(env, "AWS_ACCESS_KEY_ID="+c.creds.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+c.creds.SecretAccessKey,
			"AWS_SESSION_TOKEN="+c.creds.SessionToken, "AWS_PROFILE=")
	case c.opt.AccessKeyID != "":
		env = append(env, "AWS_ACCESS_KEY_ID="+c.opt.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+c.opt.SecretAccessKey,
			"AWS_SESSION_TOKEN=", "AWS_PROFILE=")
	case c.opt.Profile != "":
		env = append(env, "AWS_PROFILE="+c.opt.Profile)
	}
	if c.Endpoint != "" {
		env = append(env, "AWS_ENDPOINT_URL="+c.Endpoint)
		// The provider calls S3 Control at <account-id>.<endpoint host>, a name
		// that does not resolve for a local emulator. Sent through the emulator
		// as an HTTP proxy, those requests arrive anyway.
		if strings.HasPrefix(c.Endpoint, "http://") {
			env = append(env, "HTTP_PROXY="+c.Endpoint, "http_proxy="+c.Endpoint)
		}
	}
	return env
}

// EmulatorOverride is extra provider configuration for the capture's scratch
// directory when the target is an emulator. It is never committed: the
// library's code stays valid for real AWS.
func (c *Client) EmulatorOverride() []byte {
	if c.Endpoint == "" {
		return nil
	}
	return []byte("provider \"aws\" {\n  s3_use_path_style = true\n}\n")
}

// Actions a capture identity must not be allowed: a sample of writes across
// the services scriven reads, plus reading secret values.
var mustNotAllow = []string{
	"ec2:RunInstances", "ec2:TerminateInstances", "ec2:CreateVpc", "ec2:DeleteVpc", "ec2:CreateTags",
	"ec2:AuthorizeSecurityGroupIngress", "ec2:ModifyInstanceAttribute", "ec2:ReleaseAddress", "ec2:DeleteNatGateway",
	"s3:CreateBucket", "s3:DeleteBucket", "s3:PutObject", "s3:DeleteObject", "s3:PutBucketPolicy",
	"iam:CreateUser", "iam:CreateAccessKey", "iam:PutRolePolicy", "iam:AttachRolePolicy", "iam:PassRole",
	"dynamodb:PutItem", "dynamodb:DeleteTable", "sns:DeleteTopic", "sns:SetTopicAttributes",
	"lambda:UpdateFunctionCode", "lambda:InvokeFunction", "rds:DeleteDBInstance", "rds:ModifyDBInstance",
	"elasticloadbalancing:DeleteLoadBalancer", "elasticloadbalancing:ModifyListener",
	"kms:ScheduleKeyDeletion", "kms:PutKeyPolicy", "cloudformation:CreateStack", "cloudformation:DeleteStack",
	"secretsmanager:GetSecretValue", "kms:Decrypt",
}

// Actions that read stored data rather than configuration. AWS's own
// ReadOnlyAccess policy grants them, so by default an identity that has them
// is accepted with a warning; a target with metadata_only refuses it.
var dataReads = []string{
	"s3:GetObject", "dynamodb:GetItem", "dynamodb:Query", "dynamodb:Scan", "sqs:ReceiveMessage",
	"ssm:GetParameter", "logs:GetLogEvents",
}

// ReadOnly is the outcome of the read-only check.
type ReadOnly struct {
	Bad       []string // writes or secret reads the identity may make: refuse
	DataReads []string // data the identity may read (see dataReads)
	// DataChecked is false when the check could not look at data reads
	// (the dry-run fallback only covers writes).
	DataChecked bool
	Method      string // how the check was done, for catalog.yaml
}

// ReadOnlyViolations proves the identity cannot write, without writing.
//
// First choice is IAM policy simulation, which evaluates the identity's real
// policies against every action in mustNotAllow and dataReads. Where
// simulation is not available (the identity may not call it, or an emulator
// does not have it), it falls back to EC2 dry-run requests: AWS answers
// whether the call would be allowed and changes nothing.
func (c *Client) ReadOnlyViolations(ctx context.Context) (ReadOnly, error) {
	if strings.HasSuffix(c.ARN, ":root") {
		return ReadOnly{Bad: []string{"it is the account root user"}, Method: "identity"}, nil
	}
	allowed, simErr := c.simulate(ctx, append(append([]string{}, mustNotAllow...), dataReads...))
	if simErr == nil {
		r := ReadOnly{DataChecked: true,
			Method: fmt.Sprintf("IAM policy simulation of %d write, secret-read and data-read actions", len(mustNotAllow)+len(dataReads))}
		for _, a := range allowed {
			if slices.Contains(dataReads, a) {
				r.DataReads = append(r.DataReads, a)
			} else {
				r.Bad = append(r.Bad, "may "+a)
			}
		}
		return r, nil
	}
	bad, err := c.dryRun(ctx)
	if err != nil {
		return ReadOnly{}, fmt.Errorf("cannot verify the identity is read-only: IAM simulation failed (%v) and so did the dry-run check: %w",
			short(simErr), err)
	}
	return ReadOnly{Bad: bad, Method: "EC2 dry-run requests only (IAM policy simulation unavailable: " + short(simErr) + ")"}, nil
}

// simulate returns the actions the identity's policies allow.
func (c *Client) simulate(ctx context.Context, actions []string) ([]string, error) {
	src := policySourceARN(c.ARN)
	if src == "" {
		return nil, fmt.Errorf("cannot simulate policies for %s", c.ARN)
	}
	p := iam.NewSimulatePrincipalPolicyPaginator(iam.NewFromConfig(c.cfg), &iam.SimulatePrincipalPolicyInput{
		PolicySourceArn: aws.String(src), ActionNames: actions})
	var allowed []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.EvaluationResults {
			if r.EvalDecision == "allowed" {
				allowed = append(allowed, aws.ToString(r.EvalActionName))
			}
		}
	}
	return allowed, nil
}

// policySourceARN turns a caller ARN into the IAM user or role ARN that
// policy simulation accepts.
func policySourceARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return ""
	}
	res := parts[5]
	switch {
	case strings.HasPrefix(res, "user/"), strings.HasPrefix(res, "role/"):
		return arn
	case strings.HasPrefix(res, "assumed-role/"):
		seg := strings.Split(res, "/")
		if len(seg) < 2 {
			return ""
		}
		return fmt.Sprintf("%s:%s:iam::%s:role/%s", parts[0], parts[1], parts[4], seg[1])
	}
	return ""
}

func (c *Client) dryRun(ctx context.Context) ([]string, error) {
	// Every probe is a dry run of deleting something that does not exist, so
	// it stays harmless even against an emulator that ignores DryRun. AWS
	// checks permission first: an identity that may not delete is refused
	// before the ID is looked at.
	e := ec2.NewFromConfig(c.cfg, func(o *ec2.Options) { o.RetryMaxAttempts = 1 })
	yes := aws.Bool(true)
	probes := []struct {
		action string
		call   func() error
	}{
		{"ec2:DeleteVpc", func() error {
			_, err := e.DeleteVpc(ctx, &ec2.DeleteVpcInput{DryRun: yes, VpcId: aws.String("vpc-00000000000000000")})
			return err
		}},
		{"ec2:DeleteSubnet", func() error {
			_, err := e.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{DryRun: yes, SubnetId: aws.String("subnet-00000000000000000")})
			return err
		}},
		{"ec2:DeleteSecurityGroup", func() error {
			_, err := e.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{DryRun: yes, GroupId: aws.String("sg-00000000000000000")})
			return err
		}},
		{"ec2:TerminateInstances", func() error {
			_, err := e.TerminateInstances(ctx, &ec2.TerminateInstancesInput{DryRun: yes, InstanceIds: []string{"i-00000000000000000"}})
			return err
		}},
	}
	var bad []string
	for _, p := range probes {
		err := p.call()
		switch code := errorCode(err); {
		case denied(err):
		case code == "DryRunOperation", strings.Contains(code, "NotFound"), err == nil:
			// Allowed: either AWS said so, or the call got past the permission
			// check as far as looking for the (non-existent) resource.
			bad = append(bad, "may "+p.action)
		default:
			return nil, fmt.Errorf("%s dry run: %w", p.action, err)
		}
	}
	return bad, nil
}

func errorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// denied reports whether err is AWS refusing the call for lack of permission.
func denied(err error) bool {
	switch errorCode(err) {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "AuthorizationError",
		"UnauthorizedAccess", "Client.UnauthorizedOperation":
		return true
	}
	return false
}

func short(err error) string {
	if c := errorCode(err); c != "" {
		return c
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
