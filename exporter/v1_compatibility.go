package exporter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/percona/percona-toolkit/src/go/mongolib/util"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

var (
	// ErrCannotAggregateShardingChangelogEvent Cannot aggregate a new Sharding Changelog Event.
	ErrCannotAggregateShardingChangelogEvent = fmt.Errorf("failed to aggregate sharding changelog events")
	// ErrInvalidMetricValue cannot create a new metric due to an invalid value.
	ErrInvalidMetricValue = fmt.Errorf("invalid metric value")
)

/*
  This is used to convert a new metric like: mongodb_ss_asserts{assert_type=*} (1)
  to the old-compatible metric:  mongodb_mongod_asserts_total{type="regular|warning|msg|user|rollovers"}.
  In this particular case, conversion would be:
  conversion {
   newName: "mongodb_ss_asserts",
   oldName: "mongodb_mongod_asserts_total",
   labels : map[string]string{ "assert_type": "type"},
  }.

  Some other metric renaming are more complex. (2)
  In some cases, there is a total renaming, with new labels and the only part we can use to identify a metric
  is its prefix. Example:
  Metrics like mongodb_ss_metrics_operation _fastmod or
  mongodb_ss_metrics_operation_idhack or
  mongodb_ss_metrics_operation_scanAndOrder
  should use the trim the "prefix" mongodb_ss_metrics_operation from the metric name, and that remaining suffic
  is the label value for a new label "suffixLabel".
  It means that the metric (current) mongodb_ss_metrics_operation_idhack will become into the old equivalent one
  mongodb_mongod_metrics_operation_total {"state": "idhack"} as defined in the conversion slice:
   {
     oldName:     "mongodb_mongod_metrics_operation_total", //{state="fastmod|idhack|scan_and_order"}
     prefix:      "mongodb_ss_metrics_operation",           // _[fastmod|idhack|scanAndOrder]
     suffixLabel: "state",
   },

   suffixMapping field:
   --------------------
   Also, some metrics suffixes for the second renaming case need a mapping between the old and new values.
   For example, the metric mongodb_ss_wt_cache_bytes_currently_in_the_cache has mongodb_ss_wt_cache_bytes
   as the prefix so the suffix is bytes_currently_in_the_cache should be converted to a mertic named
   mongodb_mongod_wiredtiger_cache_bytes and the suffix bytes_currently_in_the_cache is being mapped to
   "total".

   Third renaming form: see (3) below.
*/

// For simple metric renaming, only some fields should be updated like the metric name, the help and some
// labels that have 1 to 1 mapping (1).
func newToOldMetric(rm *rawMetric, c conversion) *rawMetric {
	oldMetric := &rawMetric{
		fqName: c.oldName,
		help:   rm.help,
		val:    rm.val,
		vt:     rm.vt,
		ln:     make([]string, 0, len(rm.ln)),
		lv:     make([]string, 0, len(rm.lv)),
	}

	// Label values remain the same
	// copy(oldMetric.lv, rm.lv)

	for _, val := range rm.lv {
		if newLabelVal, ok := c.labelValueConversions[val]; ok {
			oldMetric.lv = append(oldMetric.lv, newLabelVal)
			continue
		}
		oldMetric.lv = append(oldMetric.lv, val)
	}

	// Some label names should be converted from the new (current) name to the
	// mongodb_exporter v1 compatible name
	for _, newLabelName := range rm.ln {
		// if it should be converted, append the old-compatible name
		if oldLabel, ok := c.labelConversions[newLabelName]; ok {
			oldMetric.ln = append(oldMetric.ln, oldLabel)
			continue
		}
		// otherwise, keep the same label name
		oldMetric.ln = append(oldMetric.ln, newLabelName)
	}

	return oldMetric
}

// The second renaming case is not a direct rename. In this case, the new metric name has a common
// prefix and the rest of the metric name is used as the value for a label in tne old metric style. (2)
// In this renaming case, the metric "mongodb_ss_wt_cache_bytes_bytes_currently_in_the_cache
// should be converted to mongodb_mongod_wiredtiger_cache_bytes with label "type": "total".
// For this conversion, we have the suffixMapping field that holds the mapping for all suffixes.
// Example definition:
//	 	oldName:     "mongodb_mongod_wiredtiger_cache_bytes",
//	 	prefix:      "mongodb_ss_wt_cache_bytes",
//	 	suffixLabel: "type",
//	 	suffixMapping: map[string]string{
//	 		"bytes_currently_in_the_cache":                           "total",
//	 		"tracked_dirty_bytes_in_the_cache":                       "dirty",
//	 		"tracked_bytes_belonging_to_internal_pages_in_the_cache": " internal_pages",
//	 		"tracked_bytes_belonging_to_leaf_pages_in_the_cache":     "internal_pages",
//	 	},
//	 },
func createOldMetricFromNew(rm *rawMetric, c conversion) *rawMetric {
	suffix := strings.TrimPrefix(rm.fqName, c.prefix)
	suffix = strings.TrimPrefix(suffix, "_")

	if newSuffix, ok := c.suffixMapping[suffix]; ok {
		suffix = newSuffix
	}

	oldMetric := &rawMetric{
		fqName: c.oldName,
		help:   c.oldName,
		val:    rm.val,
		vt:     rm.vt,
		ln:     []string{c.suffixLabel},
		lv:     []string{suffix},
	}

	return oldMetric
}

func cacheEvictedTotalMetric(m bson.M) (prometheus.Metric, error) {
	s, err := sumMetrics(m, [][]string{
		{"serverStatus", "wiredTiger", "cache", "modified pages evicted"},
		{"serverStatus", "wiredTiger", "cache", "unmodified pages evicted"},
	})
	if err != nil {
		return nil, err
	}

	d := prometheus.NewDesc("mongodb_mongod_wiredtiger_cache_evicted_total", "wiredtiger cache evicted total", nil, nil)
	metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, s)
	if err != nil {
		return nil, err
	}

	return metric, nil
}

func sumMetrics(m bson.M, paths [][]string) (float64, error) {
	var total float64

	for _, path := range paths {
		v := walkTo(m, path)
		if v == nil {
			continue
		}

		f, err := asFloat64(v)
		if err != nil {
			return 0, errors.Wrapf(ErrInvalidMetricValue, "%v", v)
		}

		total += *f
	}

	return total, nil
}

// Converts new metric to the old metric style and append it to the response slice.
func appendCompatibleMetric(res []prometheus.Metric, rm *rawMetric) []prometheus.Metric {
	compatibleMetric := metricRenameAndLabel(rm, conversions())
	if compatibleMetric == nil {
		return res
	}

	metric, err := rawToPrometheusMetric(compatibleMetric)
	if err != nil {
		invalidMetric := prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err)
		res = append(res, invalidMetric)
		return res
	}

	res = append(res, metric)

	return res
}

//nolint:funlen
func conversions() []conversion {
	return []conversion{
		{
			newName:          "mongodb_ss_asserts",
			oldName:          "mongodb_asserts_total",
			labelConversions: map[string]string{"assert_type": "type"},
		},
		{
			oldName:          "mongodb_asserts_total",
			newName:          "mongodb_ss_asserts",
			labelConversions: map[string]string{"assert_type": "type"},
		},
		{
			oldName:          "mongodb_connections",
			newName:          "mongodb_ss_connections",
			labelConversions: map[string]string{"conn_type": "state"},
		},
		{
			oldName: "mongodb_connections_metrics_created_total",
			newName: "mongodb_ss_connections_totalCreated",
		},
		{
			oldName: "mongodb_extra_info_page_faults_total",
			newName: "mongodb_ss_extra_info_page_faults",
		},
		{
			oldName: "mongodb_mongod_durability_journaled_megabytes",
			newName: "mongodb_ss_dur_journaledMB",
		},
		{
			oldName: "mongodb_mongod_durability_commits",
			newName: "mongodb_ss_dur_commits",
		},
		{
			oldName: "mongodb_mongod_background_flushing_average_milliseconds",
			newName: "mongodb_ss_backgroundFlushing_average_ms",
		},
		{
			oldName:     "mongodb_mongod_global_lock_client",
			prefix:      "mongodb_ss_globalLock_activeClients",
			suffixLabel: "type",
		},
		{
			oldName:          "mongodb_mongod_global_lock_current_queue",
			newName:          "mongodb_ss_globalLock_currentQueue",
			labelConversions: map[string]string{"count_type": "type"},
			labelValueConversions: map[string]string{
				"readers": "reader",
				"writers": "writer",
			},
		},
		{
			oldName: "mongodb_instance_local_time",
			newName: "mongodb_start",
		},

		{
			oldName: "mongodb_mongod_instance_uptime_seconds",
			newName: "mongodb_ss_uptime",
		},
		{
			oldName: "mongodb_mongod_locks_time_locked_local_microseconds_total",
			newName: "mongodb_ss_locks_Local_acquireCount_[rw]",
		},
		{
			oldName: "mongodb_memory",
			newName: "mongodb_ss_mem_[resident|virtual]",
		},
		{
			oldName:          "mongodb_mongod_metrics_cursor_open",
			newName:          "mongodb_ss_metrics_cursor_open",
			labelConversions: map[string]string{"csr_type": "state"},
		},
		{
			oldName: "mongodb_mongod_metrics_cursor_timed_out_total",
			newName: "mongodb_ss_metrics_cursor_timedOut",
		},
		{
			oldName:          "mongodb_mongod_metrics_document_total",
			newName:          "mongodb_ss_metric_document",
			labelConversions: map[string]string{"doc_op_type": "type"},
		},
		{
			oldName: "mongodb_mongod_metrics_get_last_error_wtime_num_total",
			newName: "mongodb_ss_metrics_getLastError_wtime_num",
		},
		{
			oldName: "mongodb_mongod_metrics_get_last_error_wtimeouts_total",
			newName: "mongodb_ss_metrics_getLastError_wtimeouts",
		},
		{
			oldName:     "mongodb_mongod_metrics_operation_total",
			prefix:      "mongodb_ss_metrics_operation",
			suffixLabel: "state",
		},
		{
			oldName:     "mongodb_mongod_metrics_query_executor_total",
			prefix:      "mongodb_ss_metrics_query",
			suffixLabel: "state",
		},
		{
			oldName: "mongodb_mongod_metrics_record_moves_total",
			newName: "mongodb_ss_metrics_record_moves",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_apply_batches_num_total",
			newName: "mongodb_ss_metrics_repl_apply_batches_num",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_apply_batches_total_milliseconds",
			newName: "mongodb_ss_metrics_repl_apply_batches_totalMillis",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_apply_ops_total",
			newName: "mongodb_ss_metrics_repl_apply_ops",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_buffer_count",
			newName: "mongodb_ss_metrics_repl_buffer_count",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_buffer_max_size_bytes",
			newName: "mongodb_ss_metrics_repl_buffer_maxSizeBytes",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_buffer_size_bytes",
			newName: "mongodb_ss_metrics_repl_buffer_sizeBytes",
		},
		{
			oldName:     "mongodb_mongod_metrics_repl_executor_queue",
			prefix:      "mongodb_ss_metrics_repl_executor_queues",
			suffixLabel: "type",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_executor_unsignaled_events",
			newName: "mongodb_ss_metrics_repl_executor_unsignaledEvents",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_network_bytes_total",
			newName: "mongodb_ss_metrics_repl_network_bytes",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_network_getmores_num_total",
			newName: "mongodb_ss_metrics_repl_network_getmores_num",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_network_getmores_total_milliseconds",
			newName: "mongodb_ss_metrics_repl_network_getmores_totalMillis",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_network_ops_total",
			newName: "mongodb_ss_metrics_repl_network_ops",
		},
		{
			oldName: "mongodb_mongod_metrics_repl_network_readers_created_total",
			newName: "mongodb_ss_metrics_repl_network_readersCreated",
		},
		{
			oldName: "mongodb_mongod_metrics_ttl_deleted_documents_total",
			newName: "mongodb_ss_metrics_ttl_deletedDocuments",
		},
		{
			oldName: "mongodb_mongod_metrics_ttl_passes_total",
			newName: "mongodb_ss_metrics_ttl_passes",
		},
		{
			oldName:     "mongodb_network_bytes_total",
			prefix:      "mongodb_ss_network",
			suffixLabel: "state",
		},
		{
			oldName: "mongodb_network_metrics_num_requests_total",
			newName: "mongodb_ss_network_numRequests",
		},
		{
			oldName:          "mongodb_mongod_op_counters_repl_total",
			newName:          "mongodb_ss_opcountersRepl",
			labelConversions: map[string]string{"legacy_op_type": "type"},
		},
		{
			oldName:          "mongodb_op_counters_total",
			newName:          "mongodb_ss_opcounters",
			labelConversions: map[string]string{"legacy_op_type": "type"},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_blockmanager_blocks_total",
			prefix:      "mongodb_ss_wt_block_manager",
			suffixLabel: "type",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_cache_max_bytes",
			newName: "mongodb_ss_wt_cache_maximum_bytes_configured",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_cache_overhead_percent",
			newName: "mongodb_ss_wt_cache_percentage_overhead",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_concurrent_transactions_available_tickets",
			newName: "mongodb_ss_wt_concurrentTransactions_available",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_concurrent_transactions_out_tickets",
			newName: "mongodb_ss_wt_concurrentTransactions_out",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_concurrent_transactions_total_tickets",
			newName: "mongodb_ss_wt_concurrentTransactions_totalTickets",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_log_records_scanned_total",
			newName: "mongodb_ss_wt_log_records_processed_by_log_scan",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_session_open_cursors_total",
			newName: "mongodb_ss_wt_session_open_cursor_count",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_session_open_sessions_total",
			newName: "mongodb_ss_wt_session_open_session_count",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_transactions_checkpoint_milliseconds_total",
			newName: "mongodb_ss_wt_txn_transaction_checkpoint_total_time_msecs",
		},
		{
			oldName: "mongodb_mongod_wiredtiger_transactions_running_checkpoints",
			newName: "mongodb_ss_wt_txn_transaction_checkpoint_currently_running",
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_transactions_total",
			prefix:      "mongodb_ss_wt_txn_transactions",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"begins":      "begins",
				"checkpoints": "checkpoints",
				"committed":   "committed",
				"rolled_back": "rolled_back",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_blockmanager_bytes_total",
			prefix:      "mongodb_ss_wt_block_manager",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"bytes_read": "read", "mapped_bytes_read": "read_mapped",
				"bytes_written": "written",
			},
		},
		// the 2 metrics bellow have the same prefix.
		{
			oldName:     "mongodb_mongod_wiredtiger_cache_bytes",
			prefix:      "mongodb_ss_wt_cache_bytes",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"currently_in_the_cache":                                 "total",
				"tracked_dirty_bytes_in_the_cache":                       "dirty",
				"tracked_bytes_belonging_to_internal_pages_in_the_cache": " internal_pages",
				"tracked_bytes_belonging_to_leaf_pages_in_the_cache":     "internal_pages",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_cache_bytes_total",
			prefix:      "mongodb_ss_wt_cache",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"bytes_read_into_cache":    "read",
				"bytes_written_from_cache": "written",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_cache_pages",
			prefix:      "mongodb_ss_wt_cache",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"pages_currently_held_in_the_cache": "total",
				"tracked_dirty_pages_in_the_cache":  "dirty",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_cache_pages_total",
			prefix:      "mongodb_ss_wt_cache",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"pages_read_into_cache":    "read",
				"pages_written_from_cache": "written",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_log_records_total",
			prefix:      "mongodb_ss_wt_log",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"log_records_compressed":     "compressed",
				"log_records_not_compressed": "uncompressed",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_log_bytes_total",
			prefix:      "mongodb_ss_wt_log",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"log_bytes_of_payload_data": "payload",
				"log_bytes_written":         "unwritten",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_log_operations_total",
			prefix:      "mongodb_ss_wt_log",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"log_read_operations":                  "read",
				"log_write_operations":                 "write",
				"log_scan_operations":                  "scan",
				"log_scan_records_requiring_two_reads": "scan_double",
				"log_sync_operations":                  "sync",
				"log_sync_dir_operations":              "sync_dir",
				"log_flush_operations":                 "flush",
			},
		},
		{
			oldName:     "mongodb_mongod_wiredtiger_transactions_checkpoint_milliseconds",
			prefix:      "mongodb_ss_wt_txn_transaction_checkpoint",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"min_time_msecs": "min",
				"max_time_msecs": "max",
			},
		},
		{
			oldName:          "mongodb_mongod_global_lock_current_queue",
			prefix:           "mongodb_mongod_global_lock_current_queue",
			labelConversions: map[string]string{"op_type": "type"},
		},
		{
			oldName:          "mongodb_mongod_op_latencies_ops_total",
			newName:          "mongodb_ss_opLatencies_ops",
			labelConversions: map[string]string{"op_type": "type"},
			labelValueConversions: map[string]string{
				"commands": "command",
				"reads":    "read",
				"writes":   "write",
			},
		},
		{
			oldName:          "mongodb_mongod_op_latencies_latency_total",
			newName:          "mongodb_ss_opLatencies_latency",
			labelConversions: map[string]string{"op_type": "type"},
			labelValueConversions: map[string]string{
				"commands": "command",
				"reads":    "read",
				"writes":   "write",
			},
		},
		{
			oldName:          "mongodb_mongod_metrics_document_total",
			newName:          "mongodb_ss_metrics_document",
			labelConversions: map[string]string{"doc_op_type": "state"},
		},
		{
			oldName:     "mongodb_mongod_metrics_query_executor_total",
			prefix:      "mongodb_ss_metrics_queryExecutor",
			suffixLabel: "state",
			suffixMapping: map[string]string{
				"scanned":        "scanned",
				"scannedObjects": "scanned_objects",
			},
		},
		{
			oldName:     "mongodb_memory",
			prefix:      "mongodb_ss_mem",
			suffixLabel: "type",
			suffixMapping: map[string]string{
				"resident": "resident",
				"virtual":  "virtual",
			},
		},
		{
			oldName: "mongodb_mongod_metrics_get_last_error_wtime_total_milliseconds",
			newName: "mongodb_ss_metrics_getLastError_wtime_totalMillis",
		},
		{
			oldName: "mongodb_ss_wt_cache_maximum_bytes_configured",
			newName: "mongodb_mongod_wiredtiger_cache_max_bytes",
		},
	}
}

// Third metric renaming case (3).
// Lock* metrics don't fit in (1) nor in (2) and since they are just a few, and we know they always exists
// as part of getDiagnosticData, we can just call locksMetrics with getDiagnosticData result as the input
// to get the v1 compatible metrics from the new structure.

type lockMetric struct {
	name   string
	path   []string
	labels map[string]string
}

func lockMetrics() []lockMetric {
	return []lockMetric{
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ParallelBatchWriterMode", "acquireCount", "r"},
			labels: map[string]string{"lock_mode": "r", "resource": "ParallelBatchWriterMode"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ParallelBatchWriterMode", "acquireCount", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "acquireCount", "w"},
			labels: map[string]string{"resource": "ReplicationStateTransition", "lock_mode": "w"},
		},
		{
			name:   "mongodb_ss_locks_acquireWaitCount",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "acquireCount", "W"},
			labels: map[string]string{"lock_mode": "W", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_timeAcquiringMicros",
			path:   []string{"serverStatus", "locks", "ReplicationStateTransition", "timeAcquiringMicros", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "ReplicationStateTransition"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "r"},
			labels: map[string]string{"lock_mode": "r", "resource": "Global"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "w"},
			labels: map[string]string{"lock_mode": "w", "resource": "Global"},
		},
		{
			name:   "mongodb_ss_locks_acquireCount",
			path:   []string{"serverStatus", "locks", "Global", "acquireCount", "W"},
			labels: map[string]string{"lock_mode": "W", "resource": "Global"},
		},
	}
}

// locksMetrics returns the list of lock metrics as a prometheus.Metric slice
// This function reads the human readable list from lockMetrics() and creates a slice of metrics
// ready to be exposed, taking the value for each metric from th provided bson.M structure from
// getDiagnosticData.
func locksMetrics(m bson.M) []prometheus.Metric {
	metrics := lockMetrics()
	res := make([]prometheus.Metric, 0, len(metrics))

	for _, lm := range metrics {
		mm, err := makeLockMetric(m, lm)
		if mm == nil {
			continue
		}
		if err != nil {
			logrus.Errorf("cannot convert rw metric %s to old style: %s", mm.Desc(), err)
			continue
		}
		res = append(res, mm)
	}

	return res
}

func makeLockMetric(m bson.M, lm lockMetric) (prometheus.Metric, error) {
	val := walkTo(m, lm.path)
	if val == nil {
		return nil, nil
	}

	f, err := asFloat64(val)
	if err != nil {
		return prometheus.NewInvalidMetric(prometheus.NewInvalidDesc(err), err), err
	}

	if f == nil {
		return nil, nil
	}

	ln := make([]string, 0, len(lm.labels))
	lv := make([]string, 0, len(lm.labels))
	for labelName, labelValue := range lm.labels {
		ln = append(ln, labelName)
		lv = append(lv, labelValue)
	}

	d := prometheus.NewDesc(lm.name, lm.name, ln, nil)

	return prometheus.NewConstMetric(d, prometheus.UntypedValue, *f, lv...)
}

// PMM dashboards looks for this metric so, in compatibility mode, we must expose it.
// FIXME Add it in both modes, move away from that file: https://jira.percona.com/browse/PMM-6585
func mongodbUpMetric() prometheus.Metric {
	d := prometheus.NewDesc("mongodb_up", "Whether MongoDB is up.", nil, nil)
	up, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(1))
	if err != nil {
		panic(err)
	}
	return up
}

type specialMetric struct {
	paths  [][]string
	labels map[string]string
	name   string
	help   string
}

func specialMetricDefinitions() []specialMetric {
	return []specialMetric{
		{
			name: "mongodb_mongod_locks_time_acquiring_global_microseconds_total",
			help: "sum of serverStatus.locks.Global.timeAcquiringMicros.[r|w]",
			paths: [][]string{
				{"serverStatus", "locks", "Global", "timeAcquiringMicros", "r"},
				{"serverStatus", "locks", "Global", "timeAcquiringMicros", "w"},
			},
		},
	}
}

func specialMetrics(ctx context.Context, client *mongo.Client, m bson.M, l *logrus.Logger) []prometheus.Metric {
	metrics := make([]prometheus.Metric, 0)

	for _, def := range specialMetricDefinitions() {
		val, err := sumMetrics(m, def.paths)
		if err != nil {
			l.Errorf("cannot create metric for path: %v: %s", def.paths, err)
			continue
		}

		d := prometheus.NewDesc(def.name, def.help, nil, def.labels)
		metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, val)
		if err != nil {
			l.Errorf("cannot create metric for path: %v: %s", def.paths, err)
			continue
		}

		metrics = append(metrics, metric)
	}

	metrics = append(metrics, storageEngine(m))
	metrics = append(metrics, serverVersion(m))

	ms, err := myState(ctx, client)
	if err != nil {
		l.Debugf("cannot create metric for my state: %s", err)
	} else {
		metrics = append(metrics, ms)
	}

	if ed := electionDate(m); ed != nil {
		metrics = append(metrics, ed)
	}

	if rl := replicationLag(m); rl != nil {
		metrics = append(metrics, rl)
	}

	return metrics
}

func storageEngine(m bson.M) prometheus.Metric {
	v := walkTo(m, []string{"serverStatus", "storageEngine", "name"})
	name := "mongodb_mongod_storage_engine"
	help := "The storage engine used by the MongoDB instance"

	engine, ok := v.(string)
	if !ok {
		engine = "Engine not available"
	}
	labels := map[string]string{"engine": engine}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(1))

	return metric
}

func serverVersion(m bson.M) prometheus.Metric {
	v := walkTo(m, []string{"serverStatus", "version"})
	name := "mongodb_version_info"
	help := "The server version"

	serverVersion, ok := v.(string)
	if !ok {
		serverVersion = "server version is not available"
	}
	labels := map[string]string{"mongodb": serverVersion}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(1))

	return metric
}

func myState(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	state, err := util.MyState(ctx, client)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get my state")
	}

	rs, err := util.ReplicasetConfig(ctx, client)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get replicaset config")
	}

	// 	HELP mongodb_mongod_replset_my_state An integer between 0 and 10 that represents the replica state of the current member
	// # TYPE mongodb_mongod_replset_my_state gauge
	// mongodb_mongod_replset_my_state{set="s1rs"} 1

	name := "mongodb_mongod_replset_my_state"
	help := "An integer between 0 and 10 that represents the replica state of the current member"

	labels := map[string]string{"set": rs.Config.ID}

	d := prometheus.NewDesc(name, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(state))

	return metric, nil
}

func electionDate(m bson.M) prometheus.Metric {
	replSetGetStatus, ok := m["replSetGetStatus"].(bson.M)
	if !ok {
		return nil
	}
	members, ok := replSetGetStatus["members"].(primitive.A)
	if !ok {
		return nil
	}
	for _, member := range members {
		if date, ok := member.(bson.M)["electionTime"].(primitive.Timestamp); ok {
			name := "mongodb_mongod_replset_member_election_date"
			help := "The timestamp the node was elected as replica leader"

			labels := map[string]string{"name": member.(bson.M)["name"].(string)}
			d := prometheus.NewDesc(name, help, nil, labels)
			metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(date.T))

			return metric
		}
	}

	return nil
}

func replicationLag(m bson.M) prometheus.Metric {
	var primaryTS, selfTS primitive.Timestamp
	var hasPrimary bool
	var name, statestr string

	replSetGetStatus, ok := m["replSetGetStatus"].(bson.M)
	if !ok {
		return nil
	}
	members, ok := replSetGetStatus["members"].(primitive.A)
	if !ok {
		return nil
	}
	for _, member := range members {
		if statestr, ok := member.(bson.M)["stateStr"].(string); ok && statestr == "PRIMARY" {
			if optime, ok := member.(bson.M)["optime"].(bson.M); ok {
				if ts, tok := optime["ts"].(primitive.Timestamp); tok {
					primaryTS = ts
					hasPrimary = true
				}
			}
		}

		if self, ok := member.(bson.M)["self"].(bool); ok && self {
			if optime, ok := member.(bson.M)["optime"].(bson.M); ok {
				if ts, tok := optime["ts"].(primitive.Timestamp); tok {
					selfTS = ts
					name, _ = member.(bson.M)["name"].(string)
					statestr, _ = member.(bson.M)["stateStr"].(string)
				}
			}
		}
	}

	if !hasPrimary {
		return nil
	}

	if statestr == "PRIMARY" {
		return nil
	}

	val := float64(primaryTS.T - selfTS.T)
	set, _ := replSetGetStatus["set"].(string)

	metricName := "mongodb_mongod_replset_member_replication_lag"
	help := "The replication lag that this member has with the primary."

	labels := map[string]string{
		"name":  name,
		"state": statestr,
		"set":   set,
	}
	d := prometheus.NewDesc(metricName, help, nil, labels)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, val)

	return metric
}

func mongosMetrics(ctx context.Context, client *mongo.Client, l *logrus.Logger) []prometheus.Metric {
	metrics := []prometheus.Metric{}

	if metric, err := databasesTotalPartitioned(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := databasesTotalUnpartitioned(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	metrics = append(metrics, balancerEnabled(ctx, client))

	ms, err := chunksTotal(ctx, client)
	if err != nil {
		l.Debugf("cannot create metric for collections total: %s", err)
	} else {
		metrics = append(metrics, ms...)
	}

	if metric, err := chunksBalanced(ctx, client); err != nil {
		l.Debugf("cannot create metric for chunks balanced: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	ms, err = changelog10m(ctx, client)
	if err != nil {
		l.Errorf("cannot create metric for changelog: %s", err)
	} else {
		metrics = append(metrics, ms...)
	}

	metrics = append(metrics, dbstatsMetrics(ctx, client)...)

	if metric, err := shardingShardsTotal(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	if metric, err := shardingShardsDrainingTotal(ctx, client); err != nil {
		l.Debugf("cannot create metric for database total: %s", err)
	} else {
		metrics = append(metrics, metric)
	}

	return metrics
}

func databasesTotalPartitioned(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("databases").CountDocuments(ctx, bson.M{"partitioned": true})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_databases_total"
	help := "Total number of sharded databases"
	labels := map[string]string{"type": "partitioned"}

	d := prometheus.NewDesc(name, help, nil, labels)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func databasesTotalUnpartitioned(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("databases").CountDocuments(ctx, bson.M{"partitioned": false})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_databases_total"
	help := "Total number of sharded databases"
	labels := map[string]string{"type": "unpartitioned"}

	d := prometheus.NewDesc(name, help, nil, labels)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func chunksBalanced(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	var m struct {
		InBalancerRound bool `bson:"inBalancerRound"`
	}

	cmd := bson.D{{Key: "balancerStatus", Value: "1"}}
	res := client.Database("admin").RunCommand(ctx, cmd)

	if err := res.Decode(&m); err != nil {
		return nil, err
	}

	value := float64(0)
	if !m.InBalancerRound {
		value = 1
	}

	name := "mongodb_mongos_sharding_chunks_is_balanced"
	help := "Shards are balanced"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, value)
}

func balancerEnabled(ctx context.Context, client *mongo.Client) prometheus.Metric {
	type bss struct {
		stopped bool `bson:"stopped"`
	}
	var bs bss
	enabled := 0

	err := client.Database("config").Collection("settings").FindOne(ctx, bson.M{"_id": "balancer"}).Decode(&bs)
	if err != nil {
		enabled = 1
	} else if !bs.stopped {
		enabled = 1
	}

	name := "mongodb_mongos_sharding_balancer_enabled"
	help := "Balancer is enabled"

	d := prometheus.NewDesc(name, help, nil, nil)
	metric, _ := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(enabled))

	return metric
}

func chunksTotal(ctx context.Context, client *mongo.Client) ([]prometheus.Metric, error) {
	aggregation := bson.D{
		{Key: "$group", Value: bson.M{"_id": "$shard", "count": bson.M{"$sum": 1}}},
	}

	cursor, err := client.Database("config").Collection("chunks").Aggregate(ctx, mongo.Pipeline{aggregation})
	if err != nil {
		return nil, err
	}

	var shards []bson.M
	if err = cursor.All(ctx, &shards); err != nil {
		return nil, err
	}

	metrics := make([]prometheus.Metric, 0, len(shards))

	for _, shard := range shards {
		name := "mongodb_mongos_sharding_shard_chunks_total"
		help := "Total number of chunks per shard"
		labels := map[string]string{"shard": shard["_id"].(string)}

		d := prometheus.NewDesc(name, help, nil, labels)
		val, ok := shard["count"].(int32)
		if !ok {
			continue
		}

		metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(val))
		if err != nil {
			continue
		}

		metrics = append(metrics, metric)
	}

	return metrics, nil
}

func shardingShardsTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_shards_total"
	help := "Total number of shards"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

func shardingShardsDrainingTotal(ctx context.Context, client *mongo.Client) (prometheus.Metric, error) {
	n, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{"draining": true})
	if err != nil {
		return nil, err
	}

	name := "mongodb_mongos_sharding_shards_draining_total"
	help := "Total number of drainingshards"

	d := prometheus.NewDesc(name, help, nil, nil)
	return prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(n))
}

// ShardingChangelogSummaryID Sharding Changelog Summary ID.
type ShardingChangelogSummaryID struct {
	Event string `bson:"event"`
	Note  string `bson:"note"`
}

// ShardingChangelogSummary Sharding Changelog Summary.
type ShardingChangelogSummary struct {
	ID    *ShardingChangelogSummaryID `bson:"_id"`
	Count float64                     `bson:"count"`
}

// ShardingChangelogStats is an array of Sharding changelog stats.
type ShardingChangelogStats struct {
	Items *[]ShardingChangelogSummary
}

func changelog10m(ctx context.Context, client *mongo.Client) ([]prometheus.Metric, error) {
	var metrics []prometheus.Metric

	coll := client.Database("config").Collection("changelog")
	match := bson.M{"time": bson.M{"$gt": time.Now().Add(-10 * time.Minute)}}
	group := bson.M{"_id": bson.M{"event": "$what", "note": "$details.note"}, "count": bson.M{"$sum": 1}}

	c, err := coll.Aggregate(ctx, []bson.M{{"$match": match}, {"$group": group}})
	if err != nil {
		return nil, errors.Wrap(err, ErrCannotAggregateShardingChangelogEvent.Error())
	}

	defer c.Close(ctx) //nolint:errcheck

	for c.Next(ctx) {
		s := &ShardingChangelogSummary{}
		if err := c.Decode(s); err != nil {
			log.Error(err)
			continue
		}

		name := "mongodb_mongos_sharding_changelog_10min_total"
		help := "mongodb_mongos_sharding_changelog_10min_total"

		labelValue := s.ID.Event
		if s.ID.Note != "" {
			labelValue += "." + s.ID.Note
		}

		d := prometheus.NewDesc(name, help, nil, map[string]string{"event": labelValue})
		metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, s.Count)
		if err != nil {
			continue
		}

		metrics = append(metrics, metric)
	}

	if err := c.Err(); err != nil {
		return nil, err
	}

	return metrics, nil
}

// DatabaseStatList contains stats from all databases.
type databaseStatList struct {
	Members []databaseStatus
}

// DatabaseStatus represents stats about a database (mongod and raw from mongos).
type databaseStatus struct {
	rawStatus                       // embed to collect top-level attributes
	Shards    map[string]*rawStatus `bson:"raw,omitempty"`
}

// RawStatus represents stats about a database from Mongos side.
type rawStatus struct {
	Name        string `bson:"db,omitempty"`
	IndexSize   int    `bson:"indexSize,omitempty"`
	DataSize    int    `bson:"dataSize,omitempty"`
	Collections int    `bson:"collections,omitempty"`
	Objects     int    `bson:"objects,omitempty"`
	Indexes     int    `bson:"indexes,omitempty"`
}

func getDatabaseStatList(ctx context.Context, client *mongo.Client) *databaseStatList {
	dbStatList := &databaseStatList{}
	dbNames, err := client.ListDatabaseNames(ctx, bson.M{})
	if err != nil {
		log.Errorf("Failed to get database names: %s.", err)
		return nil
	}
	for _, db := range dbNames {
		dbStatus := databaseStatus{}
		r := client.Database(db).RunCommand(context.TODO(), bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1}})
		err := r.Decode(&dbStatus)
		if err != nil {
			log.Errorf("Failed to get database status: %s.", err)
			return nil
		}
		dbStatList.Members = append(dbStatList.Members, dbStatus)
	}

	return dbStatList
}

func dbstatsMetrics(ctx context.Context, client *mongo.Client) []prometheus.Metric {
	var metrics []prometheus.Metric

	dbStatList := getDatabaseStatList(ctx, client)

	for _, member := range dbStatList.Members {
		if len(member.Shards) > 0 {
			for shard, stats := range member.Shards {
				labels := prometheus.Labels{
					"db":    stats.Name,
					"shard": strings.Split(shard, "/")[0],
				}

				name := "mongodb_mongos_db_data_size_bytes"
				help := "The total size in bytes of the uncompressed data held in this database"

				d := prometheus.NewDesc(name, help, nil, labels)
				metric, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.DataSize))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_indexes_total"
				help = "Contains a count of the total number of indexes across all collections in the database"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.Indexes))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_index_size_bytes"
				help = "The total size in bytes of all indexes created on this database"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.IndexSize))
				if err == nil {
					metrics = append(metrics, metric)
				}

				name = "mongodb_mongos_db_collections_total"
				help = "Total number of collections"

				d = prometheus.NewDesc(name, help, nil, labels)
				metric, err = prometheus.NewConstMetric(d, prometheus.GaugeValue, float64(stats.Collections))
				if err == nil {
					metrics = append(metrics, metric)
				}
			}
		}
	}

	return metrics
}

func walkTo(m primitive.M, path []string) interface{} {
	val, ok := m[path[0]]
	if !ok {
		return nil
	}

	if len(path) > 1 {
		switch v := val.(type) {
		case primitive.M:
			val = walkTo(v, path[1:])
		case map[string]interface{}:
			val = walkTo(v, path[1:])
		default:
			return nil
		}
	}

	return val
}