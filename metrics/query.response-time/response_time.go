// Copyright 2022 Block, Inc.

package queryresponsetime

import (
	"context"
	"database/sql"
	"fmt"

	myerr "github.com/go-mysql/errors"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/errors"
	"github.com/cashapp/blip/sqlutil"
)

const (
	DOMAIN = "query.response-time"

	OPT_REAL_PERCENTILES = "real-percentiles"
	OPT_TRUNCATE_TABLE   = "truncate-table"

	ERR_NO_TABLE = "table-not-exist"

	BASE_QUERY     = "SELECT ROUND(bucket_quantile * 100, 1) AS p, ROUND(bucket_timer_high / 1000000, 3) AS us FROM performance_schema.events_statements_histogram_global"
	TRUNCATE_QUERY = "TRUNCATE TABLE performance_schema.events_statements_histogram_global"
)

type percentile struct {
	formatted string // p95
	query     string
}

type qrtConfig struct {
	percentiles []percentile
	setMeta     bool
	truncate    bool
	stop        bool
	errPolicy   map[string]*errors.Policy
}

type ResponseTime struct {
	db *sql.DB
	// --
	atLevel map[string]*qrtConfig // keyed on level
}

var _ blip.Collector = &ResponseTime{}

func NewResponseTime(db *sql.DB) *ResponseTime {
	return &ResponseTime{
		db:      db,
		atLevel: map[string]*qrtConfig{},
	}
}

// Domain returns the Blip metric domain name (DOMAIN const).
func (c *ResponseTime) Domain() string {
	return DOMAIN
}

// Help returns the output for blip --print-domains.
func (c *ResponseTime) Help() blip.CollectorHelp {
	return blip.CollectorHelp{
		Domain:      DOMAIN,
		Description: "Collect metrics for query response time",
		Options: map[string]blip.CollectorHelpOption{
			OPT_REAL_PERCENTILES: {
				Name:    OPT_REAL_PERCENTILES,
				Desc:    "If real percentiles are included in meta",
				Default: "yes",
				Values: map[string]string{
					"yes": "Include real percentiles in meta",
					"no":  "Exclude real percentiles in meta",
				},
			},
			OPT_TRUNCATE_TABLE: {
				Name:    OPT_TRUNCATE_TABLE,
				Desc:    "If the source table should be truncated to reset data after each retrieval",
				Default: "yes",
				Values: map[string]string{
					"yes": "Truncate source table after each retrieval",
					"no":  "Do not truncate source table after each retrieval",
				},
			},
		},
		Metrics: []blip.CollectorMetric{
			{
				Name: "pN",
				Type: blip.GAUGE,
				Desc: "Percentile to collect where N between 1 and 999 (p99=99th, p999=99.9th)",
			},
		},
		Errors: map[string]blip.CollectorHelpError{
			ERR_NO_TABLE: {
				Name:    ERR_NO_TABLE,
				Handles: "MySQL error 1146: Table 'performance_schema.events_statements_histogram_global' doesn't exist",
				Default: errors.NewPolicy("").String(),
			},
		},
	}
}

// Prepare prepares the collector for the given plan.
func (c *ResponseTime) Prepare(ctx context.Context, plan blip.Plan) (func(), error) {
LEVEL:
	for _, level := range plan.Levels {
		dom, ok := level.Collect[DOMAIN]
		if !ok {
			continue LEVEL // not collected at this level
		}

		config := &qrtConfig{}
		if rp, ok := dom.Options[OPT_REAL_PERCENTILES]; ok && rp == "no" {
			config.setMeta = false
		} else {
			config.setMeta = true // default
		}

		if truncate, ok := dom.Options[OPT_TRUNCATE_TABLE]; ok && truncate == "no" {
			config.truncate = false
		} else {
			config.truncate = true // default
		}

		// Process list of percentiles metrics into a list of names and values
		p, err := sqlutil.PercentileMetrics(dom.Metrics)
		if err != nil {
			return nil, err
		}

		// For each percentile, save a query to fetch its (closest) value
		config.percentiles = make([]percentile, len(p))
		for i := range p {
			config.percentiles[i] = percentile{
				formatted: p[i].Name,
				query:     BASE_QUERY + fmt.Sprintf(" WHERE bucket_quantile >= %f ORDER BY bucket_quantile LIMIT 1", p[i].Value),
			}
		}

		// Apply custom error policies, if any
		config.errPolicy = map[string]*errors.Policy{}
		config.errPolicy[ERR_NO_TABLE] = errors.NewPolicy(dom.Errors[ERR_NO_TABLE])
		blip.Debug("error policy: %s=%s", ERR_NO_TABLE, config.errPolicy[ERR_NO_TABLE])

		c.atLevel[level.Name] = config
	}

	return nil, nil
}

// Collect collects metrics at the given level.
func (c *ResponseTime) Collect(ctx context.Context, levelName string) ([]blip.MetricValue, error) {
	if c.atLevel[levelName].stop {
		blip.Debug("stopped by previous error")
		return nil, nil
	}

	var metrics []blip.MetricValue
	for _, percentile := range c.atLevel[levelName].percentiles {
		var p float64
		var us float64
		err := c.db.QueryRowContext(ctx, percentile.query).Scan(&p, &us)
		if err != nil {
			return c.collectError(err, levelName, percentile.formatted)
		}

		m := blip.MetricValue{
			Type:  blip.GAUGE,
			Name:  percentile.formatted,
			Value: us,
		}
		if c.atLevel[levelName].setMeta {
			m.Meta = map[string]string{
				percentile.formatted: fmt.Sprintf("%.1f", p),
			}
		}
		metrics = append(metrics, m)
	}

	if c.atLevel[levelName].truncate {
		_, err := c.db.Exec(TRUNCATE_QUERY)
		if err != nil {
			return nil, err
		}
	}

	return metrics, nil
}

func (c *ResponseTime) collectError(err error, levelName string, metricName string) ([]blip.MetricValue, error) {
	var ep *errors.Policy
	switch myerr.MySQLErrorCode(err) {
	case 1146:
		ep = c.atLevel[levelName].errPolicy[ERR_NO_TABLE]
	default:
		return nil, err
	}

	// Stop trying to collect if error policy retry="stop". This affects
	// future calls to Collect; don't return yet because we need to check
	// the metric policy: drop or zero. If zero, we must report one zero val.
	if ep.Retry == errors.POLICY_RETRY_NO {
		c.atLevel[levelName].stop = true
	}

	// Report
	var reportedErr error
	if ep.ReportError() {
		reportedErr = err
	} else {
		blip.Debug("error policy=ignore: %v", err)
	}

	var metrics []blip.MetricValue
	if ep.Metric == errors.POLICY_METRIC_ZERO {
		metrics = []blip.MetricValue{{
			Type:  blip.GAUGE,
			Name:  metricName,
			Value: 0,
		}}
	}

	return metrics, reportedErr
}
