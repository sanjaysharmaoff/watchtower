package bootstrap

// Adapters are imported for their registration side effects only. This blank
// import block is the complete list of implementations compiled into the binary:
// adding one is a single line here, removing one is a single line here, and no
// other file changes either way.
import (
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/guard"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/llm/openai"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/llm/stub"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/prompt"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/cognition/score"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/notify"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/render"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/delivery/scheduler"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/demo"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/k8s"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/loki"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/enrich/sanitize"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/correlate"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/decode"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/ingest/gate"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/cache"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/embed/hashing"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/rerank"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/rag/vector/memory"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/storage/memstore"
	_ "github.com/sanjaysharmaoff/watchtower/internal/adapters/storage/postgres"
)
