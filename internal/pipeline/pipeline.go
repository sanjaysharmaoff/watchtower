// Package pipeline is a tiny generic stage runner.
//
// It is the backbone of the loose coupling: a stage receives a payload, returns a
// payload, and knows nothing about its neighbours. Reordering, removing or
// inserting a stage is a change to a slice literal in the composition root.
package pipeline

import (
	"context"
	"strings"
)

// Stage is one unit of work over a payload of type T.
type Stage[T any] interface {
	Name() string
	Process(ctx context.Context, in T) (T, error)
}

// Func is a stage expressed as a plain function.
type Func[T any] func(ctx context.Context, in T) (T, error)

type funcStage[T any] struct {
	name string
	fn   Func[T]
}

func (s funcStage[T]) Name() string { return s.name }

func (s funcStage[T]) Process(ctx context.Context, in T) (T, error) { return s.fn(ctx, in) }

// Of lifts a function into a named stage.
func Of[T any](name string, fn Func[T]) Stage[T] { return funcStage[T]{name: name, fn: fn} }

// Middleware decorates a stage. Logging, timeouts and panic recovery are
// middleware, so no stage has to care about them.
type Middleware[T any] func(Stage[T]) Stage[T]

// Pipeline is an ordered list of stages plus the middleware wrapped around each.
type Pipeline[T any] struct {
	name   string
	stages []Stage[T]
	mws    []Middleware[T]
}

// New builds a pipeline.
func New[T any](name string, stages ...Stage[T]) *Pipeline[T] {
	return &Pipeline[T]{name: name, stages: compact(stages)}
}

// Use registers middleware applied to every stage, outermost first.
func (p *Pipeline[T]) Use(mws ...Middleware[T]) *Pipeline[T] {
	p.mws = append(p.mws, mws...)
	return p
}

// Append adds stages to the end of the chain.
func (p *Pipeline[T]) Append(stages ...Stage[T]) *Pipeline[T] {
	p.stages = append(p.stages, compact(stages)...)
	return p
}

// Name reports the pipeline name.
func (p *Pipeline[T]) Name() string { return p.name }

// Names lists the stage names in order, handy for a startup log line.
func (p *Pipeline[T]) Names() []string {
	out := make([]string, 0, len(p.stages))
	for _, s := range p.stages {
		out = append(out, s.Name())
	}
	return out
}

// String renders the chain as "a -> b -> c".
func (p *Pipeline[T]) String() string { return strings.Join(p.Names(), " -> ") }

// Run executes every stage in order, stopping at the first error. The payload as
// mutated so far is always returned, so callers can inspect partial work.
func (p *Pipeline[T]) Run(ctx context.Context, in T) (T, error) {
	cur := in
	for _, stage := range p.stages {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		wrapped := stage
		for i := len(p.mws) - 1; i >= 0; i-- {
			wrapped = p.mws[i](wrapped)
		}
		out, err := wrapped.Process(ctx, cur)
		if err != nil {
			return cur, err
		}
		cur = out
	}
	return cur, nil
}

// compact drops nil stages so the composition root can conditionally include
// them with a plain ternary-ish expression.
func compact[T any](in []Stage[T]) []Stage[T] {
	out := make([]Stage[T], 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}
