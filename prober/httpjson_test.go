// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

import (
	"context"
	"fmt"
	"github.com/go-kit/log"
	"github.com/prometheus/blackbox_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	"gotest.tools/v3/assert"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJsonConvert_Simple(t *testing.T) {
	nested := `{
		  "parentA": "value"
		}`
	res := JsonConvert([]byte(nested), "foo")
	assert.Equal(t, len(res), 1)
	metric := res[0]
	assert.Equal(t, metric.name, "probe_httpjson_foo_parentA")
}

func TestJsonConvert_NullValue(t *testing.T) {
	nested := `{
		  "parentA": null
		}`
	res := JsonConvert([]byte(nested), "foo")
	assert.Equal(t, len(res), 0)
}

func TestJsonConvert_BooleanValue(t *testing.T) {
	nested := `{
		  "parentA": false
		}`
	res := JsonConvert([]byte(nested), "foo")
	assert.Equal(t, len(res), 1)
	metric := res[0]
	assert.Equal(t, metric.name, "probe_httpjson_foo_parentA")
	assert.Equal(t, metric.labels["parentA"], "false")
}

func TestJsonConvert_Array(t *testing.T) {
	nested := `{
		  "parentA": [
			1, 
            2,
			"three"
		  ]
		}`
	res := JsonConvert([]byte(nested), "foo")
	assert.Equal(t, len(res), 3)

	assert.Equal(t, res[0].name, "probe_httpjson_foo_parentA")
	assert.Equal(t, res[0].value, float64(1))
	assert.Equal(t, len(res[0].labels), 1)
	assert.Equal(t, GetLabelNames(res[0].labels)[0], "index_parentA")
	assert.Equal(t, res[0].labels["index_parentA"], "0")

	assert.Equal(t, res[1].name, "probe_httpjson_foo_parentA")
	assert.Equal(t, res[1].value, float64(2))
	assert.Equal(t, len(res[1].labels), 1)
	assert.Equal(t, GetLabelNames(res[1].labels)[0], "index_parentA")
	assert.Equal(t, res[1].labels["index_parentA"], "1")

	assert.Equal(t, res[2].name, "probe_httpjson_foo_parentA")
	assert.Equal(t, res[2].value, float64(1))
	assert.Equal(t, len(res[2].labels), 2)
	assert.Equal(t, res[2].labels["index_parentA"], "2")
	assert.Equal(t, res[2].labels["parentA"], "three")
}

func TestJsonConvert_Combined(t *testing.T) {
	nested := `{
		  "parentB": {
			"oneVal": 42,
			"two": [
			  1,
			  2,
			  [ {
				"inInnerArray": "innerArrayValue"
			   } ]
			],
			"three": [
			  {"a": 1},
			  {"b": 12},
				{"c": "valC"}
			]
		  },
		  "parentA": "value"
		}`
	res := JsonConvert([]byte(nested), "test")
	fmt.Println(strings.Replace(fmt.Sprintf("%v", res), "}", "},\n", -1))
	assert.Equal(t, len(res), 8)
	var keys = []string{}
	for _, re := range res {
		keys = append(keys, re.name)
	}
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentA"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_oneVal"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_three_a"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_three_b"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_three_c"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_two"))
	assert.Assert(t, Contains(keys, "probe_httpjson_test_parentB_two_inInnerArray"))
}

func TestJsonConvert_ManyObjects(t *testing.T) {
	nested, _ := os.ReadFile("testdata/servers.json")

	res := JsonConvert(nested, "server")
	assert.Equal(t, len(res), 450)
}

func TestMetricsCreated_ManyObjects(t *testing.T) {
	resultContent, _ := os.ReadFile("testdata/servers.json")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(resultContent)
		fmt.Println(resultContent)
	}))
	defer ts.Close()

	// Follow redirect, should succeed with 200.
	recorder := httptest.NewRecorder()
	registry := prometheus.NewRegistry()
	testCTX, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := ProbeHTTPJSON(testCTX, ts.URL, config.Module{Timeout: time.Second, HTTPJSON: config.HTTPJSONProbe{}}, registry, log.NewNopLogger())
	body := recorder.Body.String()
	if !result {
		t.Fatalf("Redirect test failed unexpectedly, got %s", body)
	}

	mfs, err := registry.Gather()
	assert.Equal(t, err, nil)
	expectedResults := map[string]float64{} //TODO

	checkRegistryResults(expectedResults, mfs, t)

}

func Contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}
