package guard

import (
	"regexp"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.Inspectors.Register("denylist", func(e *wire.Env) (ports.CommandInspector, error) {
		return NewDenyList(e.Cfg.Guard.DenyPatterns)
	})
	wire.Inspectors.Register("flags", func(e *wire.Env) (ports.CommandInspector, error) {
		return NewFlagList(e.Cfg.Guard.DenyFlags), nil
	})
	wire.Inspectors.Register("allowlist", func(e *wire.Env) (ports.CommandInspector, error) {
		return NewAllowList(e.Cfg.Guard.AllowBinaries), nil
	})
}

// segments splits a command line into the individual commands it would actually
// run. A model that hides `rm -rf /` behind a pipe or a `&&` gets caught, which is
// the whole point of inspecting structure instead of the raw string.
func segments(run string) []string {
	replacer := strings.NewReplacer(
		"&&", "\n", "||", "\n", ";", "\n", "|", "\n", "\r", "\n",
		"$(", "\n", "`", "\n",
	)
	raw := strings.Split(replacer.Replace(run), "\n")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(strings.Trim(s, "()"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// DenyList refuses commands matching a destructive pattern.
type DenyList struct{ rules []Rule }

// NewDenyList builds the inspector from the built-in rules plus any extras.
func NewDenyList(extra []string) (DenyList, error) {
	rules := append([]Rule(nil), denyRules...)
	for _, p := range extra {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return DenyList{}, err
		}
		rules = append(rules, Rule{Label: "configured pattern", Re: re})
	}
	return DenyList{rules: rules}, nil
}

// Name implements ports.CommandInspector.
func (DenyList) Name() string { return "denylist" }

// Inspect implements ports.CommandInspector.
//
// The whole line is checked first, because some rules are about composition
// (piping a download into a shell) and would be invisible once the line is split.
// Then each segment is checked on its own, which catches a destructive command
// hidden behind && or a subshell.
func (d DenyList) Inspect(cmd domain.Command) error {
	candidates := append([]string{cmd.Run}, segments(cmd.Run)...)
	for _, candidate := range candidates {
		for _, r := range d.rules {
			if r.Re.MatchString(candidate) {
				return domain.Block("%s (matched %q)", r.Label, strings.TrimSpace(candidate))
			}
		}
	}
	return nil
}

// FlagList refuses individual arguments that turn a recoverable action into an
// unrecoverable one.
type FlagList struct{ flags []string }

// NewFlagList builds the inspector from the built-in flags plus any extras.
func NewFlagList(extra []string) FlagList {
	flags := append([]string(nil), denyFlags...)
	for _, f := range extra {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			flags = append(flags, f)
		}
	}
	return FlagList{flags: flags}
}

// Name implements ports.CommandInspector.
func (FlagList) Name() string { return "flags" }

// Inspect implements ports.CommandInspector.
func (f FlagList) Inspect(cmd domain.Command) error {
	lower := " " + strings.ToLower(cmd.Run) + " "
	for _, flag := range f.flags {
		if strings.Contains(lower, " "+flag+" ") || strings.Contains(lower, " "+flag+"=") {
			return domain.Block("dangerous flag %q requires a human", flag)
		}
	}
	return nil
}

// AllowList refuses binaries outside the approved set. Disabled when the set is
// empty, which is how an operator opts out entirely.
type AllowList struct{ allowed map[string]struct{} }

// NewAllowList builds the inspector, defaulting to the curated diagnostic and
// remediation tool set.
func NewAllowList(binaries []string) AllowList {
	if len(binaries) == 0 {
		binaries = defaultBinaries
	}
	set := make(map[string]struct{}, len(binaries))
	for _, b := range binaries {
		if b = strings.ToLower(strings.TrimSpace(b)); b != "" {
			set[b] = struct{}{}
		}
	}
	return AllowList{allowed: set}
}

// Name implements ports.CommandInspector.
func (AllowList) Name() string { return "allowlist" }

// Inspect implements ports.CommandInspector.
func (a AllowList) Inspect(cmd domain.Command) error {
	if len(a.allowed) == 0 {
		return nil
	}
	for _, seg := range segments(cmd.Run) {
		bin := binaryOf(seg)
		if bin == "" {
			continue
		}
		if _, ok := a.allowed[bin]; !ok {
			return domain.Block("binary %q is not on the allowlist", bin)
		}
	}
	return nil
}

// binaryOf extracts the executable from a segment, skipping env assignments and
// sudo so `sudo systemctl restart x` is judged on systemctl.
func binaryOf(seg string) string {
	fields := strings.Fields(seg)
	for _, f := range fields {
		low := strings.ToLower(f)
		if strings.Contains(low, "=") && !strings.HasPrefix(low, "-") {
			continue // FOO=bar prefix
		}
		if low == "sudo" || low == "time" || low == "nohup" || low == "env" {
			continue
		}
		if idx := strings.LastIndex(low, "/"); idx >= 0 {
			low = low[idx+1:]
		}
		return low
	}
	return ""
}

// denyRules is the built-in destructive command catalogue.
var denyRules = []Rule{
	{Label: "recursive delete", Re: regexp.MustCompile(`(?i)\brm\b[^|;]*\s-{1,2}[a-z]*r`)},
	{Label: "delete of root or wide path", Re: regexp.MustCompile(`(?i)\brm\b[^|]*\s(/|/\*|/etc|/var|~|\$HOME)(\s|$)`)},
	{Label: "filesystem format", Re: regexp.MustCompile(`(?i)\b(mkfs(\.\w+)?|fdisk|parted|wipefs)\b`)},
	{Label: "raw device write", Re: regexp.MustCompile(`(?i)\bdd\b[^|]*\bof=/dev/`)},
	{Label: "redirect to block device", Re: regexp.MustCompile(`>\s*/dev/(sd|nvme|xvd)`)},
	{Label: "fork bomb", Re: regexp.MustCompile(`:\s*\(\s*\)\s*\{`)},
	{Label: "host power state change", Re: regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff|init\s+0|init\s+6)\b`)},
	{Label: "chmod/chown of a system path", Re: regexp.MustCompile(`(?i)\b(chmod|chown)\b[^|]*\s(-R\s+)?(777|/etc|/var|/usr|/)\b`)},
	{Label: "pipe from network into a shell", Re: regexp.MustCompile(`(?i)\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba)?sh\b`)},
	{Label: "namespace deletion", Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+(ns|namespace)\b`)},
	{Label: "persistent volume deletion", Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+(pv|pvc|persistentvolume\w*)\b`)},
	{Label: "cluster-wide resource deletion", Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+[^|]*(--all\b|-A\b|--all-namespaces\b)`)},
	{Label: "crd deletion", Re: regexp.MustCompile(`(?i)kubectl\s+delete\s+(crd|customresourcedefinition)`)},
	{Label: "infrastructure destroy", Re: regexp.MustCompile(`(?i)\bterraform\s+destroy\b|\bpulumi\s+destroy\b`)},
	{Label: "helm release deletion", Re: regexp.MustCompile(`(?i)\bhelm\s+(delete|uninstall)\b`)},
	{Label: "docker/container prune", Re: regexp.MustCompile(`(?i)\bdocker\s+(system\s+prune|volume\s+rm|rm\s+-f)\b`)},
	{Label: "destructive sql", Re: regexp.MustCompile(`(?i)\b(drop\s+(table|database|schema)|truncate\s+table|delete\s+from\s+\w+\s*;?\s*$)`)},
	{Label: "redis flush", Re: regexp.MustCompile(`(?i)\bflush(all|db)\b`)},
	{Label: "history or credential exfiltration", Re: regexp.MustCompile(`(?i)\b(cat|less|more|tail)\b[^|]*(/etc/shadow|id_rsa|\.aws/credentials|\.kube/config)`)},
	{Label: "git history rewrite", Re: regexp.MustCompile(`(?i)\bgit\s+(push\s+[^|]*--force|reset\s+--hard|clean\s+-[a-z]*f)`)},
	{Label: "iptables flush", Re: regexp.MustCompile(`(?i)\biptables\s+(-F|--flush)\b`)},
	{Label: "aws bulk delete", Re: regexp.MustCompile(`(?i)\baws\s+\w+\s+(delete|terminate)-\w+`)},
	{Label: "s3 recursive delete", Re: regexp.MustCompile(`(?i)\baws\s+s3\s+(rm|rb)\b[^|]*(--recursive|--force)`)},
}

// denyFlags is the built-in dangerous-argument catalogue.
var denyFlags = []string{
	"--force", "--no-preserve-root", "--grace-period=0",
	"--yes-i-really-mean-it", "--purge", "--cascade=orphan",
	"--disable-eviction", "--ignore-daemonsets", "--delete-emptydir-data",
}

// defaultBinaries is a diagnosis-heavy tool set: everything here either reads
// state or makes a change a human can undo.
var defaultBinaries = []string{
	"kubectl", "helm", "kubectx", "kubens", "stern", "argocd", "flux",
	"docker", "crictl", "systemctl", "journalctl", "service",
	"ps", "top", "htop", "free", "df", "du", "uptime", "vmstat", "iostat", "sar", "lsof", "netstat", "ss", "ip",
	"dig", "nslookup", "host", "ping", "traceroute", "mtr", "curl", "wget", "openssl", "nc",
	"cat", "head", "tail", "grep", "egrep", "awk", "sed", "sort", "uniq", "wc", "jq", "yq", "tr", "cut", "find", "ls", "stat", "echo", "date",
	"psql", "mysql", "redis-cli", "mongosh", "amqp-tools", "kafkacat", "kcat",
	"aws", "gcloud", "az", "terraform", "promtool", "amtool", "logcli", "prometheus",
	"git", "make", "go", "npm", "node", "python", "python3", "bash", "sh",
}
