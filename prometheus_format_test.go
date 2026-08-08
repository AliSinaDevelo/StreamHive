package main

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AliSinaDevelo/StreamHive/p2p"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type prometheusMetricMetadata struct {
	help     bool
	typeName string
	sample   bool
}

func TestWritePrometheusMetricsEmitsTypedMetadataForEveryHealthMetric(t *testing.T) {
	snapshot := p2p.NewTransportMetrics().Snapshot()
	for key := range (&replicationMetrics{}).Snapshot() {
		snapshot[key] = int64(len(snapshot) + 1)
	}
	for key := range (&tlsCredentialHealth{}).Snapshot(time.Unix(1_800_000_000, 0).UTC()) {
		snapshot[key] = int64(len(snapshot) + 1)
	}
	for key := range (&lifecycleRuntime{}).Metrics() {
		snapshot[key] = int64(len(snapshot) + 1)
	}

	var out bytes.Buffer
	writePrometheusMetrics(&out, snapshot)
	metadata := parsePrometheusMetricMetadata(t, out.String())

	for key := range snapshot {
		name := "streamhive_" + key
		got, ok := metadata[name]
		require.True(t, ok, "missing metadata for %s", name)
		assert.True(t, got.help, "missing HELP for %s", name)
		assert.True(t, got.sample, "missing sample for %s", name)
		assert.Equal(t, prometheusMetricType(key), got.typeName)
	}
	assert.Equal(t, "gauge", metadata["streamhive_active_peers"].typeName)
	assert.Equal(t, "gauge", metadata["streamhive_lifecycle_enabled"].typeName)
	assert.Equal(t, "gauge", metadata["streamhive_lifecycle_repair_sessions_active"].typeName)
	assert.Equal(t, "counter", metadata["streamhive_dial_errors"].typeName)
	assert.NotContains(t, out.String(), "{")
	assert.NotContains(t, out.String(), "remote=")
}

func parsePrometheusMetricMetadata(t *testing.T, exposition string) map[string]prometheusMetricMetadata {
	t.Helper()

	namePattern := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	metadata := make(map[string]prometheusMetricMetadata)
	for _, line := range strings.Split(strings.TrimSpace(exposition), "\n") {
		fields := strings.Fields(line)
		require.NotEmpty(t, fields)
		switch {
		case len(fields) >= 4 && fields[0] == "#" && fields[1] == "HELP":
			name := fields[2]
			require.Regexp(t, namePattern, name)
			entry := metadata[name]
			require.False(t, entry.help, "duplicate HELP for %s", name)
			entry.help = true
			metadata[name] = entry
		case len(fields) == 4 && fields[0] == "#" && fields[1] == "TYPE":
			name := fields[2]
			require.Regexp(t, namePattern, name)
			entry := metadata[name]
			require.Empty(t, entry.typeName, "duplicate TYPE for %s", name)
			require.Contains(t, []string{"counter", "gauge"}, fields[3])
			entry.typeName = fields[3]
			metadata[name] = entry
		default:
			require.Len(t, fields, 2, "invalid sample line %q", line)
			name := fields[0]
			require.Regexp(t, namePattern, name)
			_, err := strconv.ParseInt(fields[1], 10, 64)
			require.NoError(t, err)
			entry := metadata[name]
			require.False(t, entry.sample, "duplicate sample for %s", name)
			entry.sample = true
			metadata[name] = entry
		}
	}
	return metadata
}
