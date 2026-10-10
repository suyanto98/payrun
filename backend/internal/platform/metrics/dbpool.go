package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector membaca pgxpool.Stat() SAAT scrape (tanpa goroutine tambahan).
type poolCollector struct {
	pool                         *pgxpool.Pool
	acquired, idle, max, waits   *prometheus.Desc
	acquireSeconds, emptyAcquire *prometheus.Desc
}

// RegisterPool mendaftarkan statistik pool untuk satu service (api / worker).
func RegisterPool(service string, pool *pgxpool.Pool) {
	l := prometheus.Labels{"service": service}
	Registry.MustRegister(&poolCollector{pool: pool,
		acquired:       prometheus.NewDesc("app_db_pool_acquired_conns", "Connections currently in use.", nil, l),
		idle:           prometheus.NewDesc("app_db_pool_idle_conns", "Idle connections.", nil, l),
		max:            prometheus.NewDesc("app_db_pool_max_conns", "Pool size limit.", nil, l),
		waits:          prometheus.NewDesc("app_db_pool_acquire_total", "Successful acquires.", nil, l),
		acquireSeconds: prometheus.NewDesc("app_db_pool_acquire_wait_seconds_total", "Cumulative time spent acquiring connections.", nil, l),
		emptyAcquire:   prometheus.NewDesc("app_db_pool_empty_acquire_total", "Acquires that had to wait because the pool was empty.", nil, l),
	})
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.max, c.waits, c.acquireSeconds, c.emptyAcquire} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.acquired, prometheus.GaugeValue, float64(s.AcquiredConns()))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.IdleConns()))
	ch <- prometheus.MustNewConstMetric(c.max, prometheus.GaugeValue, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.waits, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.acquireSeconds, prometheus.CounterValue, s.AcquireDuration().Seconds())
	ch <- prometheus.MustNewConstMetric(c.emptyAcquire, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
}
