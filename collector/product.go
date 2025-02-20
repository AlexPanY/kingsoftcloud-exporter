package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/KscSDK/kingsoftcloud-exporter/config"
	"github.com/KscSDK/kingsoftcloud-exporter/constant"
	"github.com/KscSDK/kingsoftcloud-exporter/iam"
	"github.com/KscSDK/kingsoftcloud-exporter/instance"
	"github.com/KscSDK/kingsoftcloud-exporter/metric"
	"github.com/KscSDK/kingsoftcloud-exporter/util"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

//KscProductCollector
type KscProductCollector struct {
	Namespace    string
	MetricRepo   metric.MetricRepository
	InstanceRepo instance.InstanceRepository
	MetricMap    map[string]*metric.Metric
	InstanceMap  map[string]instance.KscInstance
	Queries      metric.QuerySet
	Conf         *config.KscExporterConfig
	ProductConf  *config.KscProductConfig
	handler      ProductHandler
	logger       log.Logger
	lock         sync.RWMutex
}

//GetMetrics
func (c *KscProductCollector) GetMetrics() error {
	return nil
}

// 执行所有指标的采集
func (c *KscProductCollector) Collect(ch chan<- prometheus.Metric) (err error) {
	wg := sync.WaitGroup{}

	batchSize := config.DefaultQueryMetricBatchSize
	if c.Namespace == "KS3" {
		batchSize = config.DefaultKS3QueryMetricBatchSize
	}

	queriesGroup := c.Queries.SplitByBatch(batchSize)

	wg.Add(len(queriesGroup))

	for _, queries := range queriesGroup {
		go func(q []*metric.Query) {
			defer wg.Done()
			pms, err := metric.GetPromMetricsByQueries(q, c.logger)
			if err != nil {
				level.Error(c.logger).Log(
					"msg", "Get samples fail",
					"err", err,
				)
			} else {
				for _, pm := range pms {
					ch <- pm
				}
			}
		}(queries)
	}
	wg.Wait()

	return
}

//LoadMetricsByMetricConf 指标纬度配置
func (c *KscProductCollector) LoadMetricsByMetricConf() error {
	if len(c.MetricMap) == 0 {
		c.MetricMap = make(map[string]*metric.Metric)
	}
	return nil
}

// 产品纬度配置
func (c *KscProductCollector) LoadMetricsByProductConf() error {

	level.Info(c.logger).Log("msg", "start load metrics", "Namespace", c.Namespace)
	if len(c.MetricMap) == 0 {
		c.MetricMap = make(map[string]*metric.Metric)
	}

	if err := iam.ReloadIAMProjects(c.Conf, c.logger); err != nil {
		//TODO:
	}

	instances, err := c.handler.GetInstances()
	if err != nil {
		return err
	}

	if config.IsSupportMultiDimensionNamespace(c.Namespace) {
		if len(instances) > config.DefaultSupportInstances {

			level.Warn(c.logger).Log(
				"msg",
				"loaded instances exceeds the maximum load of a single product",
				"Namespace",
				c.Namespace,
				"only_load_instances",
				config.DefaultSupportInstances,
			)

			instances = instances[:config.DefaultSupportInstances]
		}
	}

	//云服务产品是否支持多维度监控项
	if err := c.loadMetrics(instances); err != nil {
		return err
	}

	//加载查询
	var numSeries int
	currentTime := time.Now().Unix()
	for _, m := range c.MetricMap {
		if currentTime-m.LoadTimeAt < 60 {
			q, e := metric.NewQuery(m, c.MetricRepo)
			if e != nil {
				return e
			}
			c.Queries = append(c.Queries, q)
			numSeries += len(q.Metric.SeriesCache.Series)
		}
	}

	level.Info(c.logger).Log("msg", "Init new query", "Namespace", c.Namespace, "metric_num", len(c.Queries), "new_series_num", numSeries)
	return nil
}

func (c *KscProductCollector) loadMetrics(instances []instance.KscInstance) error {
	productConf, err := c.Conf.GetProductConfig(c.Namespace)
	if err != nil {
		return err
	}

	// 导出该namespace下的所有指标
	var excludeMetrics []string
	if len(productConf.ExcludeMetrics) != 0 {
		for _, em := range productConf.ExcludeMetrics {
			excludeMetrics = append(excludeMetrics, strings.ToLower(em))
		}
	}
	for _, ins := range instances {
		allMeta, err := c.MetricRepo.ListMetrics(c.Namespace, ins.GetInstanceID())
		if err != nil {
			level.Warn(c.logger).Log("msg", "request metric list fail", "err", err, "Namespace", c.Namespace, "InstanceId", ins.GetInstanceID())
		}

		if len(allMeta) > 0 {
			for _, meta := range allMeta {
				if len(excludeMetrics) != 0 && util.IsStrInList(excludeMetrics, strings.ToLower(meta.MetricName)) {
					continue
				}

				nm, err := c.createMetricWithMeta(meta, productConf, ins.GetInstanceID())
				if err != nil {
					level.Warn(c.logger).Log("msg", "Create metric fail", "err", err, "Namespace", c.Namespace, "name", meta.MetricName)
					continue
				}

				c.lock.Lock()
				key := fmt.Sprintf("%s.%s", meta.MetricName, ins.GetInstanceID())
				c.MetricMap[key] = nm
				c.lock.Unlock()

				// 获取该指标下的所有实例纬度查询或自定义纬度查询
				series, err := c.handler.GetSeriesByInstances(nm, []instance.KscInstance{ins})

				if err != nil {
					level.Error(c.logger).Log("msg", "create metric series err", "err", err, "Namespace", c.Namespace, "name", meta.MetricName)
				}

				level.Debug(c.logger).Log("msg", "found remote instances", "count", len(series), "Namespace", c.Namespace, "name", meta.MetricName)

				if err := nm.LoadSeries(series); err != nil {
					level.Error(c.logger).Log("msg", "load metric series err", "err", err, "Namespace", c.Namespace, "name", meta.MetricName)
				}
			}
		}
	}

	return nil
}

func (c *KscProductCollector) createMetricWithMeta(meta *metric.Meta, productConf config.KscProductConfig, instanceId string) (*metric.Metric, error) {
	c.lock.RLock()
	key := fmt.Sprintf("%s.%s", meta.MetricName, instanceId)
	// m, exists := c.MetricMap[meta.MetricName]
	m, exists := c.MetricMap[key]
	c.lock.RUnlock()

	if !exists {
		conf, err := metric.NewMetricConfigWithProductYaml(productConf, meta)
		if err != nil {
			return nil, err
		}

		nm, err := metric.NewMetric(meta, conf)
		if err != nil {
			return nil, err
		}
		return nm, nil
	}
	return m, nil
}

// 一个query管理一个metric的采集
func (c *KscProductCollector) initQueries() (err error) {
	var numSeries int
	for _, m := range c.MetricMap {
		q, e := metric.NewQuery(m, c.MetricRepo)
		if e != nil {
			return e
		}
		c.Queries = append(c.Queries, q)
		numSeries += len(q.Metric.SeriesCache.Series)
	}
	level.Info(c.logger).Log("msg", "Init all query ok", "Namespace", c.Namespace, "metric_num", len(c.Queries), "series_num", numSeries)
	return
}

//KscProductCollectorReloader
type KscProductCollectorReloader struct {
	collector      *KscProductCollector
	reloadInterval time.Duration
	ctx            context.Context
	cancel         context.CancelFunc
	logger         log.Logger
}

func (r *KscProductCollectorReloader) Run() {
	ticker := time.NewTicker(r.reloadInterval)
	defer ticker.Stop()

	// sleep when first start
	time.Sleep(r.reloadInterval)

	for {
		level.Info(r.logger).Log("msg", "start reload product metadata", "Namespace", r.collector.Namespace)
		e := r.reloadMetricsByProductConf()
		if e != nil {
			level.Error(r.logger).Log("msg", "reload product error", "err", e, "namespace", r.collector.Namespace)
		}
		level.Info(r.logger).Log("msg", "complete reload product metadata", "Namespace", r.collector.Namespace)
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *KscProductCollectorReloader) Stop() {
	r.cancel()
}

func (r *KscProductCollectorReloader) reloadMetricsByProductConf() error {
	return r.collector.LoadMetricsByProductConf()
}

func NewKscProductCollector(
	namespace string,
	metricRepo metric.MetricRepository,
	exporterConf *config.KscExporterConfig,
	productConf *config.KscProductConfig,
	logger log.Logger,
) (*KscProductCollector, error) {

	factory, exists := handlerFactoryMap[namespace]
	if !exists {
		return nil, fmt.Errorf("product handler not found, Namespace=%s ", namespace)
	}

	var instanceRepoCache instance.InstanceRepository

	if !util.IsStrInList(constant.NotSupportInstanceNamespaces, namespace) {
		// 支持实例自动发现的产品
		instanceRepo, err := instance.NewInstanceRepository(namespace, exporterConf, logger)
		if err != nil {
			return nil, err
		}

		// 使用instance缓存
		reloadInterval := time.Duration(productConf.ReloadIntervalMinutes * int64(time.Minute))
		instanceRepoCache = instance.NewInstanceCache(instanceRepo, reloadInterval, logger)
	}

	c := &KscProductCollector{
		Namespace:    namespace,
		MetricRepo:   metricRepo,
		InstanceRepo: instanceRepoCache,
		Conf:         exporterConf,
		ProductConf:  productConf,
		logger:       logger,
	}

	handler, err := factory(c, logger)
	if err != nil {
		return nil, err
	}
	c.handler = handler

	if err = c.LoadMetricsByMetricConf(); err != nil {
		return nil, err
	}

	if err := c.LoadMetricsByProductConf(); err != nil {
		return nil, err
	}

	return c, nil
}

//NewKscProductCollectorReloader
func NewKscProductCollectorReloader(
	ctx context.Context,
	collector *KscProductCollector,
	reloadInterval time.Duration,
	logger log.Logger,
) *KscProductCollectorReloader {
	childCtx, cancel := context.WithCancel(ctx)
	reloader := &KscProductCollectorReloader{
		collector:      collector,
		reloadInterval: reloadInterval,
		ctx:            childCtx,
		cancel:         cancel,
		logger:         logger,
	}
	return reloader
}
