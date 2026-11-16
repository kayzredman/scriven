// Package secrets resolves the references scriven.yaml holds instead of secrets:
//
//	vault://scriven/git-token         KV v2 at <mount>/data/scriven/git-token, field "value"
//	vault://scriven/smtp#password     same, field "password"
//	env://GITEA_TOKEN                 an environment variable
//	file://~/.scriven/token           a file's contents, trimmed
//
// Vault is reached at VAULT_ADDR (or vault.addr in scriven.yaml) with VAULT_TOKEN.
package secrets

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Resolver struct {
	VaultAddr  string
	VaultMount string
	client     *http.Client
}

func NewResolver(addr, mount string) *Resolver {
	if addr == "" {
		addr = os.Getenv("VAULT_ADDR")
	}
	if mount == "" {
		mount = "secret"
	}
	return &Resolver{VaultAddr: strings.TrimRight(addr, "/"), VaultMount: mount,
		client: &http.Client{Timeout: 10 * time.Second}}
}

func (r *Resolver) Resolve(ref string) (string, error) {
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env://"):
		name := strings.TrimPrefix(ref, "env://")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("%s: environment variable %s is not set", ref, name)
		}
		return v, nil
	case strings.HasPrefix(ref, "file://"):
		b, err := os.ReadFile(ExpandHome(strings.TrimPrefix(ref, "file://")))
		if err != nil {
			return "", fmt.Errorf("%s: %w", ref, err)
		}
		return strings.TrimSpace(string(b)), nil
	case strings.HasPrefix(ref, "vault://"):
		return r.vault(strings.TrimPrefix(ref, "vault://"))
	default:
		return "", fmt.Errorf("%q is not a secret reference (vault://, env:// or file://)", ref)
	}
}

func (r *Resolver) vault(p string) (string, error) {
	field := "value"
	if i := strings.Index(p, "#"); i >= 0 {
		p, field = p[:i], p[i+1:]
	}
	if r.VaultAddr == "" {
		return "", fmt.Errorf("vault://%s: VAULT_ADDR is not set", p)
	}
	token := os.Getenv("VAULT_TOKEN")
	if token == "" {
		return "", fmt.Errorf("vault://%s: VAULT_TOKEN is not set", p)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/%s/data/%s", r.VaultAddr, r.VaultMount, p), nil)
	req.Header.Set("X-Vault-Token", token)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault://%s: %w", p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("vault://%s: vault returned %s", p, resp.Status)
	}
	var body struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("vault://%s: %w", p, err)
	}
	v, ok := body.Data.Data[field]
	if !ok {
		return "", fmt.Errorf("vault://%s: no field %q", p, field)
	}
	return fmt.Sprint(v), nil
}

// ExpandHome turns a leading ~ into the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}
