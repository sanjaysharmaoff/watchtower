package wire

import "github.com/sanjaysharmaoff/watchtower/internal/ports"

// One registry per port. Every adapter in internal/adapters registers into
// exactly one of these, and the composition root only ever asks by name.
var (
	AlertDecoders    = NewRegistry[ports.AlertDecoder]("alert decoder")
	Gates            = NewRegistry[ports.Gate]("gate")
	Correlators      = NewRegistry[ports.Correlator]("correlator")
	ContextProviders = NewRegistry[ports.ContextProvider]("context provider")
	LogProcessors    = NewRegistry[ports.LogProcessor]("log processor")

	Embedders    = NewRegistry[ports.Embedder]("embedder")
	VectorStores = NewRegistry[ports.VectorStore]("vector store")
	Rerankers    = NewRegistry[ports.Reranker]("reranker")
	Caches       = NewRegistry[ports.Cache]("cache")

	Redactors       = NewRegistry[ports.Redactor]("redactor")
	RiskClassifiers = NewRegistry[ports.RiskClassifier]("risk classifier")
	PromptBuilders  = NewRegistry[ports.PromptBuilder]("prompt builder")
	Planners        = NewRegistry[ports.Planner]("planner")
	Critics         = NewRegistry[ports.Critic]("critic")
	Inspectors      = NewRegistry[ports.CommandInspector]("command inspector")
	Scorers         = NewRegistry[ports.Scorer]("scorer")
	Narrators       = NewRegistry[ports.Narrator]("narrator")

	IncidentStores    = NewRegistry[ports.IncidentStore]("incident store")
	SummaryStores     = NewRegistry[ports.SummaryStore]("summary store")
	DeadLetterQueues  = NewRegistry[ports.DeadLetterQueue]("dead letter queue")
	PartitionManagers = NewRegistry[ports.PartitionManager]("partition manager")

	Renderers  = NewRegistry[ports.Renderer]("renderer")
	Notifiers  = NewRegistry[ports.Notifier]("notifier")
	Schedulers = NewRegistry[ports.Scheduler]("scheduler")
)
