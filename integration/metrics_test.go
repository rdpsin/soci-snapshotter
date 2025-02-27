/*
   Copyright The Soci Snapshotter Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/soci-snapshotter/fs/layer"
	commonmetrics "github.com/awslabs/soci-snapshotter/fs/metrics/common"
	"github.com/awslabs/soci-snapshotter/soci"

	shell "github.com/awslabs/soci-snapshotter/util/dockershell"
	"github.com/awslabs/soci-snapshotter/util/testutil"
	"github.com/awslabs/soci-snapshotter/ztoc"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	tcpMetricsAddress  = "localhost:1338"
	unixMetricsAddress = "/var/lib/soci-snapshotter-grpc/metrics.sock"
	metricsPath        = "/metrics"
)

const tcpMetricsConfig = `
metrics_address="` + tcpMetricsAddress + `"
`

const unixMetricsConfig = `
metrics_address="` + unixMetricsAddress + `"
metrics_network="unix"
`

func TestMetrics(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		command []string
	}{
		{
			name:    "tcp",
			config:  tcpMetricsConfig,
			command: []string{"curl", "--fail", tcpMetricsAddress + metricsPath},
		},
		{
			name:    "unix",
			config:  unixMetricsConfig,
			command: []string{"curl", "--fail", "--unix-socket", unixMetricsAddress, "http://localhost" + metricsPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh, done := newSnapshotterBaseShell(t)
			defer done()
			rebootContainerd(t, sh, "", tt.config)
			sh.X(tt.command...)
			if err := sh.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}

}

func TestOverlayFallbackMetric(t *testing.T) {
	sh, done := newSnapshotterBaseShell(t)
	defer done()

	testCases := []struct {
		name                  string
		image                 string
		indexDigestFn         func(*shell.Shell, imageInfo) string
		expectedFallbackCount int
	}{
		{
			name:  "image with all layers having ztocs and no fs.Mount error results in 0 overlay fallback",
			image: rabbitmqImage,
			indexDigestFn: func(sh *shell.Shell, image imageInfo) string {
				return buildIndex(sh, image, withMinLayerSize(0))
			},
			expectedFallbackCount: 0,
		},
		{
			name:  "image with some layers not having ztoc and no fs.Mount results in 0 overlay fallback",
			image: rabbitmqImage,
			indexDigestFn: func(sh *shell.Shell, image imageInfo) string {
				return buildIndex(sh, image, withMinLayerSize(defaultMinLayerSize))
			},
			expectedFallbackCount: 0,
		},
		{
			name:  "image with fs.Mount errors results in non-zero overlay fallback",
			image: rabbitmqImage,
			indexDigestFn: func(_ *shell.Shell, _ imageInfo) string {
				return "dwadwadawdad"
			},
			expectedFallbackCount: 10,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rebootContainerd(t, sh, getContainerdConfigToml(t, false), getSnapshotterConfigToml(t, false, tcpMetricsConfig))

			imgInfo := dockerhub(tc.image)

			sh.X("nerdctl", "pull", "-q", imgInfo.ref)
			indexDigest := tc.indexDigestFn(sh, imgInfo)

			sh.X("soci", "image", "rpull", "--soci-index-digest", indexDigest, imgInfo.ref)
			curlOutput := string(sh.O("curl", tcpMetricsAddress+metricsPath))

			if err := checkOverlayFallbackCount(curlOutput, tc.expectedFallbackCount); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFuseOperationFailureMetrics(t *testing.T) {
	const logFuseOperationConfig = `
[fuse]
log_fuse_operations = true
`

	sh, done := newSnapshotterBaseShell(t)
	defer done()

	manipulateZtocMetadata := func(zt *ztoc.Ztoc) {
		for i, md := range zt.FileMetadata {
			md.UncompressedOffset += 2
			md.UncompressedSize = math.MaxInt64
			md.Xattrs = map[string]string{"foo": "bar"}
			zt.FileMetadata[i] = md
		}
	}

	testCases := []struct {
		name                       string
		image                      string
		indexDigestFn              func(*testing.T, *shell.Shell, imageInfo) string
		metricToCheck              string
		expectFuseOperationFailure bool
	}{
		{
			name:  "image with valid ztocs and index doesn't cause fuse file.read failures",
			image: rabbitmqImage,
			indexDigestFn: func(t *testing.T, sh *shell.Shell, image imageInfo) string {
				return buildIndex(sh, image, withMinLayerSize(0))
			},
			// even a valid index/ztoc produces some fuse operation failures such as
			// node.lookup and node.getxattr failures, so we only check a specific fuse failure metric.
			metricToCheck:              commonmetrics.FuseFileReadFailureCount,
			expectFuseOperationFailure: false,
		},
		{
			name:  "image with valid-formatted but invalid-data ztocs causes fuse file.read failures",
			image: rabbitmqImage,
			indexDigestFn: func(t *testing.T, sh *shell.Shell, image imageInfo) string {
				indexDigest, err := buildIndexByManipulatingZtocData(sh, buildIndex(sh, image, withMinLayerSize(0)), manipulateZtocMetadata)
				if err != nil {
					t.Fatal(err)
				}
				return indexDigest
			},
			metricToCheck:              commonmetrics.FuseFileReadFailureCount,
			expectFuseOperationFailure: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rebootContainerd(t, sh, getContainerdConfigToml(t, false), getSnapshotterConfigToml(t, false, tcpMetricsConfig, logFuseOperationConfig))

			imgInfo := dockerhub(tc.image)
			sh.X("nerdctl", "pull", "-q", imgInfo.ref)
			indexDigest := tc.indexDigestFn(t, sh, imgInfo)

			sh.X("soci", "image", "rpull", "--soci-index-digest", indexDigest, imgInfo.ref)
			// this command may fail due to fuse operation failure, use XLog to avoid crashing shell
			sh.XLog("ctr", "run", "--rm", "--snapshotter=soci", imgInfo.ref, "test", "echo", "hi")

			curlOutput := string(sh.O("curl", tcpMetricsAddress+metricsPath))
			checkFuseOperationFailureMetrics(t, curlOutput, tc.metricToCheck, tc.expectFuseOperationFailure)
		})
	}
}

func TestFuseOperationCountMetrics(t *testing.T) {
	const snapshotterConfig = `
fuse_metrics_emit_wait_duration_sec = 10
	`

	sh, done := newSnapshotterBaseShell(t)
	defer done()

	testCases := []struct {
		name  string
		image string
	}{
		{
			name:  "rabbitmq image",
			image: rabbitmqImage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rebootContainerd(t, sh, getContainerdConfigToml(t, false), getSnapshotterConfigToml(t, false, tcpMetricsConfig, snapshotterConfig))

			imgInfo := dockerhub(tc.image)
			sh.X("nerdctl", "pull", "-q", imgInfo.ref)
			indexDigest := buildIndex(sh, imgInfo)

			sh.X("soci", "image", "rpull", "--soci-index-digest", indexDigest, imgInfo.ref)
			sh.XLog("ctr", "run", "-d", "--snapshotter=soci", imgInfo.ref, "test", "echo", "hi")

			curlOutput := string(sh.O("curl", tcpMetricsAddress+metricsPath))

			for _, m := range layer.FuseOpsList {
				if checkMetricExists(curlOutput, m) {
					t.Fatalf("got unexpected metric: %s", m)
				}
			}

			time.Sleep(10 * time.Second)
			curlOutput = string(sh.O("curl", tcpMetricsAddress+metricsPath))
			for _, m := range layer.FuseOpsList {
				if !checkMetricExists(curlOutput, m) {
					t.Fatalf("missing expected metric: %s", m)
				}
			}
		})
	}

}

func TestBackgroundFetchMetrics(t *testing.T) {
	const backgroundFetchConfig = `
[background_fetch]
silence_period_msec = 1000
fetch_period_msec = 100
emit_metric_period_sec = 2
	`

	bgFetchMetricsToCheck := []string{
		commonmetrics.BackgroundFetchWorkQueueSize,
		commonmetrics.BackgroundSpanFetchCount,
	}

	sh, done := newSnapshotterBaseShell(t)
	defer done()

	testCases := []struct {
		name  string
		image string
	}{
		{
			name:  "drupal image",
			image: drupalImage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rebootContainerd(t, sh, getContainerdConfigToml(t, false), getSnapshotterConfigToml(t, false, tcpMetricsConfig, backgroundFetchConfig))

			imgInfo := dockerhub(tc.image)
			sh.X("nerdctl", "pull", "-q", imgInfo.ref)
			indexDigest := buildIndex(sh, imgInfo)

			sh.X("soci", "image", "rpull", "--soci-index-digest", indexDigest, imgInfo.ref)
			sh.XLog("ctr", "run", "-d", "--snapshotter=soci", imgInfo.ref, "test", "echo", "hi")

			time.Sleep(5 * time.Second)
			curlOutput := string(sh.O("curl", tcpMetricsAddress+metricsPath))
			for _, m := range bgFetchMetricsToCheck {
				if !checkMetricExists(curlOutput, m) {
					t.Fatalf("missing expected metric: %s", m)
				}
			}
		})
	}

}

// buildIndexByManipulatingZtocData produces a new soci index by manipulating
// the ztocs of an existing index specified by `indexDigest`.
//
// The new index (and ztocs) are stored separately and the original index keeps unchanged.
// The manipulated ztocs are (de)serializable but have meaningless ztoc data (manipuated by `manipulator`).
// This helps test soci behaviors when ztocs have valid format but wrong/corrupted data.
func buildIndexByManipulatingZtocData(sh *shell.Shell, indexDigest string, manipulator func(*ztoc.Ztoc)) (string, error) {
	index, err := sociIndexFromDigest(sh, indexDigest)
	if err != nil {
		return "", err
	}

	var ztocDescs []ocispec.Descriptor
	for _, blob := range index.Blobs {
		ztocDigest := blob.Digest.String()
		blobContent := fetchContentFromPath(sh, blobStorePath+"/"+trimSha256Prefix(ztocDigest))
		zt, err := ztoc.Unmarshal(bytes.NewReader(blobContent))
		if err != nil {
			return "", fmt.Errorf("invalid ztoc %s from soci index %s: %v", ztocDigest, indexDigest, err)
		}

		// manipulate the ztoc
		manipulator(zt)

		ztocReader, ztocDesc, err := ztoc.Marshal(zt)
		if err != nil {
			return "", fmt.Errorf("unable to marshal ztoc %s: %s", ztocDesc.Digest.String(), err)
		}
		ztocBytes, err := io.ReadAll(ztocReader)
		if err != nil {
			return "", fmt.Errorf("unable to read bytes of ztoc %s: %s", ztocDesc.Digest.String(), err)
		}

		ztocPath := fmt.Sprintf("%s/%s", blobStorePath, trimSha256Prefix(ztocDesc.Digest.String()))
		if err := testutil.WriteFileContents(sh, ztocPath, ztocBytes, 0600); err != nil {
			return "", fmt.Errorf("cannot write ztoc %s to path %s: %s", ztocDesc.Digest.String(), ztocPath, err)
		}

		ztocDesc.MediaType = soci.SociLayerMediaType
		ztocDesc.Annotations = blob.Annotations
		ztocDescs = append(ztocDescs, ztocDesc)
	}

	newIndex := soci.Index{
		MediaType:    ocispec.MediaTypeArtifactManifest,
		ArtifactType: soci.SociIndexArtifactType,
		Blobs:        ztocDescs,
		Subject: &ocispec.Descriptor{
			MediaType: ocispec.MediaTypeArtifactManifest,
			Digest:    index.Subject.Digest,
			Size:      index.Subject.Size,
		},
	}

	b, err := json.Marshal(newIndex)
	if err != nil {
		return "", err
	}

	newIndexDigest := digest.FromBytes(b)
	newIndexPath := fmt.Sprintf("%s/%s", blobStorePath, trimSha256Prefix(newIndexDigest.String()))
	if err := testutil.WriteFileContents(sh, newIndexPath, b, 0600); err != nil {
		return "", fmt.Errorf("cannot write index %s to path %s: %s", newIndexDigest, newIndexPath, err)
	}
	return strings.Trim(newIndexDigest.String(), "\n"), nil
}

// checkFuseOperationFailureMetrics checks if output from metrics endpoint includes
// a specific fuse operation failure metrics (or any fuse op failure if an empty string is given)
func checkFuseOperationFailureMetrics(t *testing.T, output string, metricToCheck string, expectOpFailure bool) {
	metricCountSum := 0

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		// skip non-fuse and fuse_mount_failure_count metrics
		if !strings.Contains(line, "fuse") || strings.Contains(line, commonmetrics.FuseMountFailureCount) {
			continue
		}

		parts := strings.Split(line, " ")
		if metricCount, err := strconv.Atoi(parts[len(parts)-1]); err == nil && metricCount != 0 {
			t.Logf("fuse operation failure metric: %s", line)
			if metricToCheck == "" || strings.Contains(line, metricToCheck) {
				metricCountSum += metricCount
			}
		}
	}

	if (metricCountSum != 0) != expectOpFailure {
		t.Fatalf("incorrect fuse operation failure metrics. metric: %s, total operation failure count: %d, expect fuse operation failure: %t",
			metricToCheck, metricCountSum, expectOpFailure)
	}
}

func checkMetricExists(output, metric string) bool {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, metric) {
			return true
		}
	}
	return false
}
