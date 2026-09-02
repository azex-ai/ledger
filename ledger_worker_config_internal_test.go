package ledger

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/service"
)

// TestMergeWorkerConfig_FillsEveryField is the guardrail the previous round
// was missing. mergeWorkerConfig used to be sixteen hand-written if
// statements, and when AttestInterval/AttestBatchSize were added to
// service.WorkerConfig nothing connected "two new fields" to "two more if
// statements": the attestation job's interval stayed zero, runLoop skipped
// it, and the P6 chain never advanced for a full release. The remedy that
// shipped was adding the two missing cases -- the shape that produced them
// was left in place.
//
// This asserts the general property instead of the two instances: after
// merging into an entirely zero-valued config, EVERY field equals its
// DefaultWorkerConfig counterpart. Adding a seventeenth field that
// mergeWorkerConfig cannot fill fails here, by name, immediately.
func TestMergeWorkerConfig_FillsEveryField(t *testing.T) {
	defaults := service.DefaultWorkerConfig()
	merged := mergeWorkerConfig(service.WorkerConfig{})

	dv := reflect.ValueOf(defaults)
	mv := reflect.ValueOf(merged)
	typ := dv.Type()

	require.Greater(t, typ.NumField(), 10,
		"sanity: service.WorkerConfig should have the full job configuration on it; this test is not looking at the real type")

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		require.False(t, mv.Field(i).IsZero(),
			"mergeWorkerConfig left service.WorkerConfig.%s at its zero value. A zero interval makes "+
				"service/worker.go skip that job entirely and a zero batch size makes it process nothing -- "+
				"either way the job silently does not run while DefaultWorkerConfig advertises that it does. "+
				"Teach mergeWorkerConfig's switch about this field's kind (%s).",
			field.Name, field.Type)
		require.Equal(t, dv.Field(i).Interface(), mv.Field(i).Interface(),
			"mergeWorkerConfig filled service.WorkerConfig.%s with something other than its default", field.Name)
	}
}

// TestMergeWorkerConfig_KeepsCallerValues is the control: a merge that simply
// overwrote everything with the defaults would satisfy the test above while
// discarding the configuration the caller asked for.
func TestMergeWorkerConfig_KeepsCallerValues(t *testing.T) {
	defaults := service.DefaultWorkerConfig()

	in := service.WorkerConfig{}
	iv := reflect.ValueOf(&in).Elem()
	dv := reflect.ValueOf(defaults)

	// Double every int-kind field, so each carries a value distinguishable
	// from its default, then assert every one survives the merge.
	for i := 0; i < iv.NumField(); i++ {
		f := iv.Field(i)
		if f.Kind() >= reflect.Int && f.Kind() <= reflect.Int64 && f.CanSet() {
			f.SetInt(dv.Field(i).Int() * 2)
		}
	}

	merged := mergeWorkerConfig(in)
	mv := reflect.ValueOf(merged)
	typ := mv.Type()
	for i := 0; i < typ.NumField(); i++ {
		if !typ.Field(i).IsExported() {
			continue
		}
		require.Equal(t, iv.Field(i).Interface(), mv.Field(i).Interface(),
			"mergeWorkerConfig overwrote the caller's explicit value for service.WorkerConfig.%s", typ.Field(i).Name)
	}
}
