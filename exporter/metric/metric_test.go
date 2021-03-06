// Copyright 2020, Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metric

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/label"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/number"
	export "go.opentelemetry.io/otel/sdk/export/metric"
	"go.opentelemetry.io/otel/sdk/export/metric/metrictest"
	aggtest "go.opentelemetry.io/otel/sdk/metric/aggregator/aggregatortest"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/lastvalue"
	"go.opentelemetry.io/otel/sdk/metric/controller/push"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/googleinterns/cloud-operations-api-mock/cloudmock"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/option"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
)

var (
	formatter = func(d *metric.Descriptor) string {
		return fmt.Sprintf("test.googleapis.com/%s", d.Name())
	}
)

func TestExportMetrics(t *testing.T) {
	cloudMock := cloudmock.NewCloudMock()
	defer cloudMock.Shutdown()

	clientOpt := option.WithGRPCConn(cloudMock.ClientConn())
	res := &resource.Resource{}
	cps := metrictest.NewCheckpointSet(res)
	ctx := context.Background()
	desc := metric.NewDescriptor("testing", metric.ValueRecorderInstrumentKind, number.Float64Kind)

	lvagg := &lastvalue.New(1)[0]
	aggtest.CheckedUpdate(t, lvagg, number.NewFloat64Number(12.34), &desc)
	lvagg.Update(ctx, number.NewFloat64Number(12.34), &desc)
	cps.Add(&desc, lvagg, label.String("a", "A"), label.String("b", "B"))

	opts := []Option{
		WithProjectID("PROJECT_ID_NOT_REAL"),
		WithMonitoringClientOptions(clientOpt),
		WithMetricDescriptorTypeFormatter(formatter),
	}

	exporter, err := NewRawExporter(opts...)
	if err != nil {
		t.Errorf("Error occurred when creating exporter: %v", err)
	}

	err = exporter.Export(ctx, cps)
	assert.NoError(t, err)
}

func TestExportCounter(t *testing.T) {
	cloudMock := cloudmock.NewCloudMock()
	defer cloudMock.Shutdown()

	clientOpt := option.WithGRPCConn(cloudMock.ClientConn())

	resOpt := push.WithResource(
		resource.NewWithAttributes(
			label.String("test_id", "abc123"),
		),
	)

	pusher, err := InstallNewPipeline(
		[]Option{
			WithProjectID("PROJECT_ID_NOT_REAL"),
			WithMonitoringClientOptions(clientOpt),
			WithMetricDescriptorTypeFormatter(formatter),
		},
		resOpt,
	)
	assert.NoError(t, err)

	defer pusher.Stop()

	// Start meter
	ctx := context.Background()
	meter := pusher.MeterProvider().Meter("cloudmonitoring/test")

	// Register counter value
	counter := metric.Must(meter).NewInt64Counter("counter-a")
	clabels := []label.KeyValue{label.Key("key").String("value")}
	counter.Add(ctx, 100, clabels...)
}

func TestDescToMetricType(t *testing.T) {
	inMe := []*metricExporter{
		{
			o: &options{},
		},
		{
			o: &options{
				MetricDescriptorTypeFormatter: formatter,
			},
		},
	}

	inDesc := []metric.Descriptor{
		metric.NewDescriptor("testing", metric.ValueRecorderInstrumentKind, number.Float64Kind),
		metric.NewDescriptor("test/of/path", metric.ValueRecorderInstrumentKind, number.Float64Kind),
	}

	wants := []string{
		"custom.googleapis.com/opentelemetry/testing",
		"test.googleapis.com/test/of/path",
	}

	for i, w := range wants {
		out := inMe[i].descToMetricType(&inDesc[i])
		if out != w {
			t.Errorf("expected: %v, actual: %v", w, out)
		}
	}
}

func TestRecordToMpb(t *testing.T) {
	res := &resource.Resource{}
	cps := metrictest.NewCheckpointSet(res)
	ctx := context.Background()

	desc := metric.NewDescriptor("testing", metric.ValueRecorderInstrumentKind, number.Float64Kind)

	lvagg := &lastvalue.New(1)[0]
	aggtest.CheckedUpdate(t, lvagg, number.NewFloat64Number(12.34), &desc)
	err := lvagg.Update(ctx, 1, &desc)
	if err != nil {
		t.Errorf("%v", err)
	}
	cps.Add(&desc, lvagg, label.String("a", "A"), label.String("b.b", "B"))

	md := &googlemetricpb.MetricDescriptor{
		Name:        desc.Name(),
		Type:        fmt.Sprintf(cloudMonitoringMetricDescriptorNameFormat, desc.Name()),
		MetricKind:  googlemetricpb.MetricDescriptor_GAUGE,
		ValueType:   googlemetricpb.MetricDescriptor_DOUBLE,
		Description: "test",
	}

	mdkey := key{
		name:        md.Name,
		libraryname: "",
	}
	me := &metricExporter{
		o: &options{},
		mdCache: map[key]*googlemetricpb.MetricDescriptor{
			mdkey: md,
		},
	}

	want := &googlemetricpb.Metric{
		Type: md.Type,
		Labels: map[string]string{
			"a":   "A",
			"b_b": "B",
		},
	}

	aggError := cps.ForEach(export.CumulativeExportKindSelector(), func(r export.Record) error {
		out := me.recordToMpb(&r)
		if !reflect.DeepEqual(want, out) {
			return fmt.Errorf("expected: %v, actual: %v", want, out)
		}
		return nil
	})
	if aggError != nil {
		t.Errorf("%v", aggError)
	}
}

func TestResourceToMonitoredResourcepb(t *testing.T) {
	testCases := []struct {
		resource       *resource.Resource
		expectedType   string
		expectedLabels map[string]string
	}{
		// k8s_container
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("cloud.zone", "us-central1-a"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
				label.String("k8s.namespace.name", "default"),
				label.String("k8s.pod.name", "opentelemetry-pod-autoconf"),
				label.String("container.name", "opentelemetry"),
			),
			"k8s_container",
			map[string]string{
				"project_id":     "",
				"location":       "us-central1-a",
				"cluster_name":   "opentelemetry-cluster",
				"namespace_name": "default",
				"pod_name":       "opentelemetry-pod-autoconf",
				"container_name": "opentelemetry",
			},
		},
		// k8s_node
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("cloud.zone", "us-central1-a"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
				label.String("host.name", "opentelemetry-node"),
			),
			"k8s_node",
			map[string]string{
				"project_id":   "",
				"location":     "us-central1-a",
				"cluster_name": "opentelemetry-cluster",
				"node_name":    "opentelemetry-node",
			},
		},
		// k8s_pod
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("cloud.zone", "us-central1-a"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
				label.String("k8s.namespace.name", "default"),
				label.String("k8s.pod.name", "opentelemetry-pod-autoconf"),
			),
			"k8s_pod",
			map[string]string{
				"project_id":     "",
				"location":       "us-central1-a",
				"cluster_name":   "opentelemetry-cluster",
				"namespace_name": "default",
				"pod_name":       "opentelemetry-pod-autoconf",
			},
		},
		// k8s_cluster
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("cloud.zone", "us-central1-a"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
			),
			"k8s_cluster",
			map[string]string{
				"project_id":   "",
				"location":     "us-central1-a",
				"cluster_name": "opentelemetry-cluster",
			},
		},
		// k8s_node missing a field
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
				label.String("host.name", "opentelemetry-node"),
			),
			"global",
			map[string]string{
				"project_id": "",
			},
		},
		// nonexisting resource types
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "none"),
				label.String("cloud.zone", "us-central1-a"),
				label.String("k8s.cluster.name", "opentelemetry-cluster"),
				label.String("k8s.namespace.name", "default"),
				label.String("k8s.pod.name", "opentelemetry-pod-autoconf"),
				label.String("container.name", "opentelemetry"),
			),
			"global",
			map[string]string{
				"project_id": "",
			},
		},
		// GCE resource fields
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("host.id", "123"),
				label.String("cloud.zone", "us-central1-a"),
			),
			"gce_instance",
			map[string]string{
				"project_id":  "",
				"instance_id": "123",
				"zone":        "us-central1-a",
			},
		},
		// AWS resources
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "aws"),
				label.String("cloud.region", "us-central1-a"),
				label.String("host.id", "123"),
				label.String("cloud.account.id", "fake_account"),
			),
			"aws_ec2_instance",
			map[string]string{
				"project_id":  "",
				"instance_id": "123",
				"region":      "us-central1-a",
				"aws_account": "fake_account",
			},
		},
		// Cloud Run
		{
			resource.NewWithAttributes(
				label.String("cloud.provider", "gcp"),
				label.String("cloud.region", "utopia"),
				label.String("service.instance.id", "bar"),
				label.String("service.name", "x-service"),
				label.String("service.namespace", "cloud-run-managed"),
			),
			"generic_task",
			map[string]string{
				"project_id": "",
				"location":   "utopia",
				"namespace":  "cloud-run-managed",
				"job":        "x-service",
				"task_id":    "bar",
			},
		},
	}

	desc := metric.NewDescriptor("testing", metric.ValueRecorderInstrumentKind, number.Float64Kind)

	md := &googlemetricpb.MetricDescriptor{
		Name:        desc.Name(),
		Type:        fmt.Sprintf(cloudMonitoringMetricDescriptorNameFormat, desc.Name()),
		MetricKind:  googlemetricpb.MetricDescriptor_GAUGE,
		ValueType:   googlemetricpb.MetricDescriptor_DOUBLE,
		Description: "test",
	}

	mdkey := key{
		name:        md.Name,
		libraryname: "",
	}

	me := &metricExporter{
		o: &options{},
		mdCache: map[key]*googlemetricpb.MetricDescriptor{
			mdkey: md,
		},
	}

	for _, test := range testCases {
		t.Run(test.resource.String(), func(t *testing.T) {
			got := me.resourceToMonitoredResourcepb(test.resource)
			if !reflect.DeepEqual(got.GetLabels(), test.expectedLabels) {
				t.Errorf("expected: %v, actual: %v", test.expectedLabels, got.GetLabels())
			}
			if got.GetType() != test.expectedType {
				t.Errorf("expected: %v, actual: %v", test.expectedType, got.GetType())
			}
		})
	}
}
