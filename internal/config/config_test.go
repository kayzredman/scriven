package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# keep me
version: 1
scope:
  pilot: rancher/prod-sim
  targets:
    - platform: rancher
      id: prod-sim
      env: prod
      kubeconfig: ~/.kube/scriven
library:
  provider: gitea
  url: http://localhost:3000/scriven/iac-library.git
  credentials: vault://scriven/git-token
notify:
  channels: [teams]
  teams_webhook: vault://scriven/teams-webhook
merge_policy:
  default: {approvals: 1}
`

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scriven.yaml")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid sample rejected: %v", err)
	}
	if tg, _ := c.Target(""); tg.ID != "prod-sim" {
		t.Fatalf("pilot target = %q", tg.ID)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	if _, err := Load(write(t, sample+"notfy: {}\n")); err == nil {
		t.Fatal("typo in a key should be an error")
	}
}

func TestValidateCatchesRawSecretsAndBadPilot(t *testing.T) {
	c, _ := Load(write(t, sample))
	c.Library.Credentials = "ghp_raw_token_value"
	c.Scope.Pilot = "rancher/nope"
	c.MergePolicy["dev"] = MergeRule{Approvals: 1, AutoMerge: true}
	err := c.Validate()
	for _, want := range []string{"library.credentials", "scope.pilot", "merge_policy.dev"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected error about %s, got %v", want, err)
		}
	}
}

func TestSetKeepsCommentsAndParsesYAML(t *testing.T) {
	p := write(t, sample)
	c, raw, err := Set(p, "notify.channels", "[teams, email]")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Notify.Channels) != 2 || c.Notify.Channels[1] != "email" {
		t.Fatalf("channels = %v", c.Notify.Channels)
	}
	if !strings.Contains(string(raw), "# keep me") {
		t.Fatal("comment lost")
	}
	if _, _, err := Set(p, "merge_policy.prod.approvals", "2"); err != nil {
		t.Fatalf("creating a nested key: %v", err)
	}
	if _, _, err := Set(p, "scope.targets.5.id", "x"); err == nil {
		t.Fatal("out-of-range index should fail")
	}
	if _, _, err := Set(p, "library.bogus", "x"); err == nil {
		t.Fatal("unknown key should fail")
	}
}

func TestValidateAWSTarget(t *testing.T) {
	base := func() *Config {
		c, err := Load(write(t, sample))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ok := base()
	ok.Scope.Targets = append(ok.Scope.Targets, Target{Platform: "aws", ID: "shop", Env: "dev", Region: "eu-west-1",
		AccessKeyID: "vault://scriven/aws#access_key_id", SecretAccessKey: "vault://scriven/aws#secret_access_key"})
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid aws target rejected: %v", err)
	}
	for name, tgt := range map[string]Target{
		"region: required":       {Platform: "aws", ID: "a", Env: "dev"},
		"not a raw key":          {Platform: "aws", ID: "a", Env: "dev", Region: "r", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "env://S"},
		"or neither":             {Platform: "aws", ID: "a", Env: "dev", Region: "r", AccessKeyID: "env://K"},
		"profile or access keys": {Platform: "aws", ID: "a", Env: "dev", Region: "r", Profile: "p", AccessKeyID: "env://K", SecretAccessKey: "env://S"},
	} {
		c := base()
		c.Scope.Targets = append(c.Scope.Targets, tgt)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("want an error mentioning %q, got %v", name, err)
		}
	}
}
