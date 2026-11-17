package config

import (
	"strings"
	"testing"
)

func TestBaseBranch(t *testing.T) {
	c := &Config{Library: Library{}, MergePolicy: map[string]MergeRule{
		"default": {Approvals: 1},
		"dev":     {AutoMerge: true, Branch: "dev-library"},
	}}
	if got := c.BaseBranch("dev"); got != "dev-library" {
		t.Errorf("dev: got %q", got)
	}
	if got := c.BaseBranch("prod"); got != "main" {
		t.Errorf("prod (default rule, no branch): got %q", got)
	}
	c.Library.Branch = "trunk"
	if got := c.BaseBranch("prod"); got != "trunk" {
		t.Errorf("prod with library.branch: got %q", got)
	}
}

func TestValidateDecisions(t *testing.T) {
	no := false
	base := func() *Config {
		c, err := Load(write(t, sample))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	aws := func(id, region string) Target {
		return Target{Platform: "aws", ID: id, Env: "dev", Region: region, Profile: "sandbox"}
	}

	c := base()
	c.MergePolicy["dev"] = MergeRule{AutoMerge: true, Branch: "observed-dev"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "merge_policy.dev.branch") {
		t.Errorf("reserved branch name accepted: %v", err)
	}

	c = base()
	c.Scope.Targets = append(c.Scope.Targets, aws("eu", "eu-west-1"), aws("us", "us-east-1"))
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "global_resources") {
		t.Errorf("two targets capturing one account's IAM accepted: %v", err)
	}
	c.Scope.Targets[2].GlobalResources = &no
	if err := c.Validate(); err != nil {
		t.Errorf("with global_resources: false on one: %v", err)
	}

	c = base()
	r := aws("eu", "eu-west-1")
	r.RoleARN, r.ExternalID = "scriven-readonly", "x"
	c.Scope.Targets = append(c.Scope.Targets, r)
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "role_arn") {
		t.Errorf("role_arn that is not an ARN accepted: %v", err)
	}
}
