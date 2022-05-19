// Copyright 2016 The Prometheus Authors
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
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/blackbox_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"golang.org/x/net/publicsuffix"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func ProbeHTTPJSON(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, logger log.Logger) (success bool) {

	jsonBody, reqSucc := doRequest(ctx, target, module, logger)
	if !reqSucc {
		level.Error(logger).Log("msg", "Error executing request")
	}
	userPrefix := module.Params.Get("userPrefix")
	if userPrefix == "" {
		userPrefix = module.HTTPJSON.UserPrefix
		if userPrefix == "" {
			userPrefix = "object"
		}
	}
	jsonMetrics := JsonConvert(jsonBody, userPrefix)

	gauges := map[string]*prometheus.GaugeVec{}
	for i := 0; i < len(jsonMetrics); i++ {
		jsonMetric := jsonMetrics[i]
		if len(jsonMetric.labels) == 0 {
			newGauge := prometheus.NewGauge(prometheus.GaugeOpts{
				Name: jsonMetric.name,
				Help: "Gauge for a JSON property",
			})
			newGauge.Set(jsonMetric.value)
			registry.MustRegister(newGauge)
		} else {
			gauge, ok := gauges[jsonMetric.name]
			if !ok {
				gauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
					Name: jsonMetric.name,
					Help: "Gauge for a JSON property",
				}, GetLabelNames(jsonMetric.labels))
				registry.MustRegister(gauge)
				gauges[jsonMetric.name] = gauge
			}
			gauge.With(jsonMetric.labels).Set(jsonMetric.value)
		}
	}
	return reqSucc
}

func doRequest(ctx context.Context, target string, module config.Module, logger log.Logger) ([]byte, bool) {

	httpJsonConfig := module.HTTPJSON

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	targetURL, err := url.Parse(target)
	if err != nil {
		level.Error(logger).Log("msg", "Could not parse target URL", "err", err)
		return nil, false
	}

	targetHost := targetURL.Hostname()
	targetPort := targetURL.Port()

	httpClientConfig := module.HTTPJSON.HTTPClientConfig
	if len(httpClientConfig.TLSConfig.ServerName) == 0 {
		// If there is no `server_name` in tls_config, use
		// the hostname of the target.
		httpClientConfig.TLSConfig.ServerName = targetHost

		// However, if there is a Host header it is better to use
		// its value instead. This helps avoid TLS handshake error
		// if targetHost is an IP address.
		for name, value := range httpJsonConfig.Headers {
			if textproto.CanonicalMIMEHeaderKey(name) == "Host" {
				httpClientConfig.TLSConfig.ServerName = value
			}
		}
	}
	client, err := pconfig.NewClientFromConfig(httpClientConfig, "http_probe", pconfig.WithKeepAlivesDisabled())
	if err != nil {
		level.Error(logger).Log("msg", "Error generating HTTP client", "err", err)
		return nil, false
	}

	httpClientConfig.TLSConfig.ServerName = ""
	noServerName, err := pconfig.NewRoundTripperFromConfig(httpClientConfig, "http_probe", pconfig.WithKeepAlivesDisabled())
	if err != nil {
		level.Error(logger).Log("msg", "Error generating HTTP client without ServerName", "err", err)
		return nil, false
	}

	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		level.Error(logger).Log("msg", "Error generating cookiejar", "err", err)
		return nil, false
	}
	client.Jar = jar

	// Inject transport that tracks traces for each redirect,
	// and does not set TLS ServerNames on redirect if needed.
	tt := newTransport(client.Transport, noServerName, logger)
	client.Transport = tt

	var redirects int
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		level.Info(logger).Log("msg", "Received redirect", "location", r.Response.Header.Get("Location"))
		redirects = len(via)
		if redirects > 10 || !httpJsonConfig.HTTPClientConfig.FollowRedirects {
			level.Info(logger).Log("msg", "Not following redirect")
			return errors.New("don't follow redirects")
		}
		return nil
	}

	if httpJsonConfig.Method == "" {
		httpJsonConfig.Method = "GET"
	}

	if targetPort != "" && !strings.Contains(targetURL.Host, ":"+targetPort) {
		targetURL.Host = net.JoinHostPort(targetURL.Host, targetPort)
	}

	var body io.Reader

	// If a body is configured, add it to the request.
	if httpJsonConfig.Body != "" {
		body = strings.NewReader(httpJsonConfig.Body)
	}

	request, err := http.NewRequest(httpJsonConfig.Method, targetURL.String(), body)
	if err != nil {
		level.Error(logger).Log("msg", "Error creating request", "err", err)
		return nil, false
	}
	request.Host = targetURL.Host
	request = request.WithContext(ctx)

	for key, value := range httpJsonConfig.Headers {
		if textproto.CanonicalMIMEHeaderKey(key) == "Host" {
			request.Host = value
			continue
		}
		request.Header.Set(key, value)
	}

	_, hasUserAgent := request.Header["User-Agent"]
	if !hasUserAgent {
		request.Header.Set("User-Agent", userAgentDefaultHeader)
	}

	level.Debug(logger).Log("msg", "Performing request: ", "url", request.URL.String())
	resp, err := client.Do(request)

	if resp != nil && err == nil {
		level.Info(logger).Log("msg", "Received HTTP response", "status_code", resp.StatusCode)
		byteCounter := &byteCounter{ReadCloser: resp.Body}
		body, err := ioutil.ReadAll(byteCounter)
		return body, err == nil && resp.StatusCode < 300
	}
	level.Error(logger).Log("msg", "Error receiving HTTP response", "err", err)
	return nil, false
}

func GetLabelNames(labels prometheus.Labels) []string {
	keys := []string{}
	for k := range labels {
		keys = append(keys, k)
	}
	return keys
}

type JsonMetric struct {
	name   string
	labels prometheus.Labels
	value  float64
}

func JsonConvert(inputJson []byte, userPrefix string) []JsonMetric {
	intermediate := make(map[string]interface{})
	err := json.Unmarshal(inputJson, &intermediate)
	if err != nil {
		return nil
	}
	var result []JsonMetric
	Flatten("probe_httpjson_"+userPrefix, intermediate, &result)
	return result
}

func Flatten(prefix string, src map[string]interface{}, dest *[]JsonMetric) {
	flattenRecursive(prefix, src, dest, prometheus.Labels{})
}

func flattenRecursive(collectedMetricsName string, src map[string]interface{}, dest *[]JsonMetric, collectedLabels prometheus.Labels) {
	const sep = "_"
	if len(collectedMetricsName) > 0 {
		collectedMetricsName += sep
	}
	iterateInOrder(src, func(currentPropName string, v interface{}) {
		childLabels := copyLabels(collectedLabels)
		switch child := v.(type) {
		case map[string]interface{}:
			flattenRecursive(collectedMetricsName+currentPropName, child, dest, childLabels)
		case []interface{}:
			flattenArrayRecursive(collectedMetricsName+currentPropName, currentPropName, child, dest, childLabels)
		default:
			createJsonMetric(collectedMetricsName+currentPropName, v, childLabels, currentPropName, dest)
		}
	})
}

func iterateInOrder(src map[string]interface{}, visitor func(key string, value interface{})) {
	// create slice and store keys
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	// sort the slice by keys
	sort.Strings(keys)
	// iterate by sorted keys
	for _, key := range keys {
		visitor(key, src[key])
	}
}

func flattenArrayRecursive(collectedMetricsName string, currentPropName string, src []interface{}, dest *[]JsonMetric, collectedLabels prometheus.Labels) {
	for i := 0; i < len(src); i++ {
		childLabels := copyLabels(collectedLabels)
		childLabels["index_"+currentPropName] = fmt.Sprintf("%v", i)
		switch arrayEle := src[i].(type) {
		case map[string]interface{}:
			flattenRecursive(collectedMetricsName, arrayEle, dest, childLabels)
		case []interface{}:
			flattenArrayRecursive(collectedMetricsName, currentPropName+"_i", arrayEle, dest, childLabels)
		default:
			createJsonMetric(collectedMetricsName, src[i], childLabels, currentPropName, dest)
		}
	}
}

func createJsonMetric(collectedMetricsName string, v interface{}, collectedLabels prometheus.Labels, currentPropName string, dest *[]JsonMetric) {
	if v == nil {
		return
	}
	mappedValue := processValue(v, &collectedLabels, currentPropName)
	var slice = *dest
	*dest = append(slice, JsonMetric{
		name:   collectedMetricsName,
		labels: collectedLabels,
		value:  mappedValue,
	})
}

func copyLabels(collectedLabels prometheus.Labels) prometheus.Labels {
	childLabels := make(prometheus.Labels)
	for k, v := range collectedLabels {
		childLabels[k] = v
	}
	return childLabels
}

func processValue(value interface{}, labels *prometheus.Labels, propName string) float64 {
	switch typeOfValue := value.(type) {
	case bool, string:
		(*labels)[propName] = fmt.Sprintf("%v", typeOfValue)
		return 1
	default:
		converted, _ := strconv.ParseFloat(fmt.Sprintf("%v", value), 64)
		return converted
	}
}
