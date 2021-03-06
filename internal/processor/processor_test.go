package processor

import (
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"github.com/DataDog/datadog-firehose-nozzle/internal/metric"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var (
	mchan chan []metric.MetricPackage
	p     *Processor
)

var _ = Describe("MetricProcessor", func() {
	BeforeEach(func() {
		mchan = make(chan []metric.MetricPackage, 1500)
		p, _ = NewProcessor(mchan, []string{}, "", false,
			nil, 4, 0, nil)
	})

	It("processes value & counter metrics", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp:  1000000000,
			InstanceId: "123",
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"valueName": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
					},
				},
			},
		})
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp:  2000000000,
			InstanceId: "123",
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Counter{
				Counter: &loggregator_v2.Counter{
					Name:  "counterName",
					Delta: uint64(6),
					Total: uint64(11),
				},
			},
		})

		var metricPkg1 []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg1))

		var metricPkg2 []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg2))

		metricPkgs := append(metricPkg1, metricPkg2...)

		Expect(metricPkgs).To(HaveLen(4))
		for _, m := range metricPkgs {
			Expect(m.MetricValue.Tags).To(ContainElement("instance_id:123"))
			if m.MetricKey.Name == "valueName" || m.MetricKey.Name == "origin.valueName" {
				Expect(m.MetricValue.Points).To(Equal([]metric.Point{{Timestamp: 1, Value: 5.0}}))
			} else if m.MetricKey.Name == "counterName" || m.MetricKey.Name == "origin.counterName" {
				Expect(m.MetricValue.Points).To(Equal([]metric.Point{{Timestamp: 2, Value: 11.0}}))
			} else {
				panic("unknown metric in package: " + m.MetricKey.Name)
			}
		}
	})

	It("generates metrics twice: once with origin in name, once without", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"fooMetric": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
					},
				},
			},
		})

		var metricPkg []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg))

		Expect(metricPkg).To(HaveLen(2))

		legacyFound := false
		newFound := false
		for _, m := range metricPkg {
			if m.MetricKey.Name == "origin.fooMetric" {
				legacyFound = true
			} else if m.MetricKey.Name == "fooMetric" {
				newFound = true
			}
		}
		Expect(legacyFound).To(BeTrue())
		Expect(newFound).To(BeTrue())
	})

	It("extracts multiple values from Gauge if it has multiple metrics", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"fooMetric": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
						"barMetric": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(6),
						},
					},
				},
			},
		})

		var metricPkg []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg))

		Expect(metricPkg).To(HaveLen(4))

		legacyFooFound := false
		legacyBarFound := false
		newFooFound := false
		newBarFound := false
		for _, m := range metricPkg {
			if m.MetricKey.Name == "origin.fooMetric" {
				legacyFooFound = true
				Expect(m.MetricValue.Points[0].Value).To(Equal(5.0))
			} else if m.MetricKey.Name == "fooMetric" {
				newFooFound = true
				Expect(m.MetricValue.Points[0].Value).To(Equal(5.0))
			} else if m.MetricKey.Name == "origin.barMetric" {
				legacyBarFound = true
				Expect(m.MetricValue.Points[0].Value).To(Equal(6.0))
			} else if m.MetricKey.Name == "barMetric" {
				newBarFound = true
				Expect(m.MetricValue.Points[0].Value).To(Equal(6.0))
			}
		}
		Expect(legacyFooFound).To(BeTrue())
		Expect(newFooFound).To(BeTrue())
		Expect(legacyBarFound).To(BeTrue())
		Expect(newBarFound).To(BeTrue())
	})

	It("adds a new alias for `bosh-hm-forwarder` metrics", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"bosh-hm-forwarder.foo": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
					},
				},
			},
		})

		var metricPkg []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg))

		Expect(metricPkg).To(HaveLen(3))

		legacyFound := false
		newFound := false
		boshAliasFound := false
		for _, m := range metricPkg {
			if m.MetricKey.Name == "origin.bosh-hm-forwarder.foo" {
				legacyFound = true
			} else if m.MetricKey.Name == "bosh-hm-forwarder.foo" {
				newFound = true
			} else if m.MetricKey.Name == "bosh.healthmonitor.foo" {
				boshAliasFound = true
			}
		}
		Expect(legacyFound).To(BeTrue())
		Expect(newFound).To(BeTrue())
		Expect(boshAliasFound).To(BeTrue())
	})

	It("ignores messages that aren't value metrics or counter events", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Log{
				Log: &loggregator_v2.Log{
					Payload: []byte("log message"),
					Type:    loggregator_v2.Log_OUT,
				},
			},
		})
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp:  1000000000,
			SourceId:   "app-id",
			InstanceId: "4",
			Tags: map[string]string{
				"origin":     "origin",
				"deployment": "deployment-name",
				"job":        "doppler",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"cpu": &loggregator_v2.GaugeValue{
							Unit:  "gauge",
							Value: float64(20.0),
						},
						"memory": &loggregator_v2.GaugeValue{
							Unit:  "gauge",
							Value: float64(19939949),
						},
						"disk": &loggregator_v2.GaugeValue{
							Unit:  "gauge",
							Value: float64(29488929),
						},
						"memory_quota": &loggregator_v2.GaugeValue{
							Unit:  "gauge",
							Value: float64(19939949),
						},
						"disk_quota": &loggregator_v2.GaugeValue{
							Unit:  "gauge",
							Value: float64(29488929),
						},
					},
				},
			},
		})

		Consistently(mchan).ShouldNot(Receive())
	})

	It("adds tags", func() {
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			SourceId:  "some.source",
			Tags: map[string]string{
				"origin":     "test-origin",
				"deployment": "deployment-name-aaaaaaaaaaaaaaaaaaaa",
				"job":        "doppler-partition-aaaaaaaaaaaaaaaaaaaa",
				"ip":         "10.0.1.2",
				"protocol":   "http",
				"request_id": "a1f5-deadbeef",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"fooMetric": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
					},
				},
			},
		})

		var metricPkg []metric.MetricPackage
		Eventually(mchan).Should(Receive(&metricPkg))

		Expect(metricPkg).To(HaveLen(2))
		for _, m := range metricPkg {
			Expect(m.MetricValue.Tags).To(Equal([]string{
				"deployment:deployment-name",
				"deployment:deployment-name-aaaaaaaaaaaaaaaaaaaa",
				"ip:10.0.1.2",
				"job:doppler",
				"job:doppler-partition-aaaaaaaaaaaaaaaaaaaa",
				"name:test-origin",
				"origin:test-origin",
				"protocol:http",
				"request_id:a1f5-deadbeef",
				"source_id:some.source",
			}))
		}

		// Check it does the correct dogate tag replacements when env_name and index are set
		p.environment = "env_name"
		p.ProcessMetric(&loggregator_v2.Envelope{
			Timestamp: 1000000000,
			Tags: map[string]string{
				"origin":     "test-origin",
				"deployment": "deployment-name-aaaaaaaaaaaaaaaaaaaa",
				"job":        "doppler-partition-aaaaaaaaaaaaaaaaaaaa",
				"ip":         "10.0.1.2",
				"protocol":   "http",
				"request_id": "a1f5-deadbeef",
				"index":      "1",
			},
			Message: &loggregator_v2.Envelope_Gauge{
				Gauge: &loggregator_v2.Gauge{
					Metrics: map[string]*loggregator_v2.GaugeValue{
						"fooMetric": &loggregator_v2.GaugeValue{
							Unit:  "counter",
							Value: float64(5),
						},
					},
				},
			},
		})

		Eventually(mchan).Should(Receive(&metricPkg))

		Expect(metricPkg).To(HaveLen(2))
		for _, m := range metricPkg {
			Expect(m.MetricValue.Tags).To(Equal([]string{
				"deployment:deployment-name",
				"deployment:deployment-name-aaaaaaaaaaaaaaaaaaaa",
				"deployment:deployment-name_env_name",
				"env:env_name",
				"index:1",
				"ip:10.0.1.2",
				"job:doppler",
				"job:doppler-partition-aaaaaaaaaaaaaaaaaaaa",
				"name:test-origin",
				"origin:test-origin",
				"protocol:http",
				"request_id:a1f5-deadbeef",
			}))
		}
	})

	Context("custom tags", func() {
		BeforeEach(func() {
			mchan = make(chan []metric.MetricPackage, 1500)
			p, _ = NewProcessor(mchan, []string{"environment:foo", "foundry:bar"}, "", false,
				nil, 4, 0, nil)
		})

		It("adds custom tags to infra metrics", func() {
			p.ProcessMetric(&loggregator_v2.Envelope{
				Timestamp: 1000000000,
				Tags: map[string]string{
					"origin":     "test-origin",
					"deployment": "deployment-name",
					"job":        "doppler",
					"ip":         "10.0.1.2",
					"protocol":   "http",
					"request_id": "a1f5-deadbeef",
					"index":      "1",
				},
				Message: &loggregator_v2.Envelope_Gauge{
					Gauge: &loggregator_v2.Gauge{
						Metrics: map[string]*loggregator_v2.GaugeValue{
							"fooMetric": &loggregator_v2.GaugeValue{
								Unit:  "counter",
								Value: float64(5),
							},
						},
					},
				},
			})

			var metricPkg []metric.MetricPackage
			Eventually(mchan).Should(Receive(&metricPkg))

			Expect(metricPkg).To(HaveLen(2))
			for _, metric := range metricPkg {
				Expect(metric.MetricValue.Tags).To(Equal([]string{
					"deployment:deployment-name",
					"environment:foo",
					"foundry:bar",
					"index:1",
					"ip:10.0.1.2",
					"job:doppler",
					"name:test-origin",
					"origin:test-origin",
					"protocol:http",
					"request_id:a1f5-deadbeef",
				}))
			}
		})
		// custom tags on app metrics tested in app_metrics_test
		// custom tags on internal metrics tested in datadogclient_test
	})
})
