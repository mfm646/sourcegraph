package coursier

import (
	"fmt"

	"github.com/sourcegraph/sourcegraph/internal/metrics"
	"github.com/sourcegraph/sourcegraph/internal/observation"
)

type Operations struct {
	fetchSources  *observation.Operation
	exists        *observation.Operation
	fetchByteCode *observation.Operation
	runCommand    *observation.Operation
}

func NewOperationsMetrics(observationContext *observation.Context) *metrics.OperationMetrics {
	return metrics.NewOperationMetrics(
		observationContext.Registerer,
		"codeintel_coursier",
		metrics.WithLabels("op"),
		metrics.WithCountHelp("Total number of method invocations."),
	)
}

func NewOperationsFromMetrics(observationContext *observation.Context, metrics *metrics.OperationMetrics) *Operations {
	op := func(name string) *observation.Operation {
		return observationContext.Operation(observation.Op{
			Name:         fmt.Sprintf("codeintel.coursier.%s", name),
			MetricLabels: []string{name},
			Metrics:      metrics,
		})
	}

	return &Operations{
		fetchSources:  op("FetchSources"),
		exists:        op("Exists"),
		fetchByteCode: op("FetchByteCode"),
		runCommand:    op("RunCommand"),
	}
}
