package guard

import (
	"regexp"
	"strings"

	"github.com/sanjaysharmaoff/watchtower/internal/domain"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

func init() {
	wire.RiskClassifiers.Register("heuristic", func(e *wire.Env) (ports.RiskClassifier, error) {
		return Classifier{}, nil
	})
}

// Classifier decides a command's risk from its text. The model's own claim is
// ignored: it is the party with the incentive to understate.
type Classifier struct{}

// Name implements ports.RiskClassifier.
func (Classifier) Name() string { return "heuristic" }

var (
	reDangerous = regexp.MustCompile(`(?i)\b(rm|mkfs|dd|shutdown|reboot|halt|poweroff|destroy|drop|truncate|flushall|flushdb|wipefs|prune)\b|--force|--purge|--grace-period=0`)
	reMutating  = regexp.MustCompile(`(?i)\b(apply|create|patch|edit|replace|scale|autoscale|set|rollout|restart|stop|start|enable|disable|delete|evict|cordon|uncordon|drain|taint|annotate|label|exec|cp|upgrade|install|rollback|resize|kill|update|insert|alter|reload)\b`)
	reReadOnly  = regexp.MustCompile(`(?i)^(get|describe|logs|top|events|explain|version|status|list|show|cat|head|tail|less|more|grep|egrep|awk|sed|sort|uniq|wc|jq|yq|df|du|free|ps|uptime|vmstat|iostat|sar|lsof|netstat|ss|dig|nslookup|host|ping|traceroute|mtr|echo|date|find|stat|ls)$`)
)

// Classify implements ports.RiskClassifier. Segments are judged independently and
// the worst one wins, so a read wrapped around a delete is not read.
func (c Classifier) Classify(cmd domain.Command) domain.RiskLevel {
	worst := domain.RiskRead
	promote := func(level domain.RiskLevel) {
		switch level {
		case domain.RiskDangerous:
			worst = domain.RiskDangerous
		case domain.RiskMutate:
			if worst != domain.RiskDangerous {
				worst = domain.RiskMutate
			}
		}
	}

	for _, seg := range segments(cmd.Run) {
		switch {
		case reDangerous.MatchString(seg):
			promote(domain.RiskDangerous)
		case reMutating.MatchString(seg):
			promote(domain.RiskMutate)
		case isReadOnly(seg):
			// leave as read
		default:
			// Unknown shapes are treated as state-changing. Understating risk is the
			// expensive mistake here; overstating it only costs the reviewer a glance.
			promote(domain.RiskMutate)
		}
	}
	return worst
}

// isReadOnly checks the subcommand as well as the binary, since `kubectl get` and
// `kubectl delete` share a binary but not a risk level.
func isReadOnly(seg string) bool {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return true
	}
	if reReadOnly.MatchString(strings.ToLower(trimPath(fields[0]))) {
		return true
	}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		return reReadOnly.MatchString(strings.ToLower(f))
	}
	return false
}

func trimPath(s string) string {
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		return s[idx+1:]
	}
	return s
}
