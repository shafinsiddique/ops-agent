// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build integration_test

/*
The launcher command launches a VM and begins a soak test on it.

Specifically, it installs the Ops Agent and a Python program that
logs to a specific file that the Ops Agent is watching.

This command is configured by the following environment variables,
in addition to the ones at the top of gce_testing.go:

LOG_RATE: How many log entries per second to send to the Ops Agent.

LOG_SIZE_IN_BYTES: How many bytes each log entry should be.

TTL: How long to keep the VM alive, expressed as "24h30m" or similar.

DISTRO: The GCE image family name to run, e.g. "debian-11".

VM_NAME: (Optional) The name of the VM to spawn. If not supplied,
a random name will be generated by gce_testing.go.

For example, after replacing `my_project` with a real project, you
could run it like:

```
PROJECT=my_project \
  DISTRO=debian-11 \
  ZONES=us-central1-b \
  TTL=100m \
  LOG_SIZE_IN_BYTES=1000 \
  LOG_RATE=1000 \
	go run -tags=integration_test .
```
*/

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/ops-agent/integration_test/agents"
	"github.com/GoogleCloudPlatform/ops-agent/integration_test/gce"
)

var (
	logSizeInBytes   = os.Getenv("LOG_SIZE_IN_BYTES")
	logRate          = os.Getenv("LOG_RATE")
	logPath          = "/tmp/tail_file"
	logGeneratorPath = "/log_generator.py"

	ttl    = os.Getenv("TTL")
	distro = os.Getenv("DISTRO")
	vmName = os.Getenv("VM_NAME")
)

//go:embed log_generator.py
var logGeneratorSource string

func main() {
	if err := mainErr(); err != nil {
		log.Fatal(err)
	}
}

func mainErr() error {
	defer gce.CleanupKeysOrDie()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	// Log to stderr.
	logger := log.Default()

	parsedTTL, err := time.ParseDuration(ttl)
	if err != nil {
		return fmt.Errorf("Could not parse TTL duration %q: %w", ttl, err)
	}

	// Create the VM.
	options := gce.VMOptions{
		Platform:    distro,
		Name:        vmName,
		MachineType: "e2-standard-16",
		Labels: map[string]string{
			"ttl": strconv.Itoa(int(parsedTTL / time.Minute)),
		},
		Metadata: map[string]string{
			// This is to avoid Windows updates and reboots (b/295165549), and
			// also to avoid throughput blips when the OS Config agent runs
			// periodically.
			"osconfig-disabled-features": "tasks",
		},
		ExtraCreateArguments: []string{"--boot-disk-size=4000GB"},
	}
	vm, err := gce.CreateInstance(ctx, logger, options)
	if err != nil {
		return err
	}
	debugLogPath := "/tmp/log_generator.log"

	// Install the Ops Agent with a config telling it to watch logPath,
	// and debugLogPath for debugging.
	config := fmt.Sprintf(`logging:
  receivers:
    mylog_source:
      type: files
      include_paths:
      - %s
    generator_debug_logs:
      type: files
      include_paths:
      - %s
  exporters:
    google:
      type: google_cloud_logging
  service:
    pipelines:
      my_pipeline:
        receivers:
        - mylog_source
        - generator_debug_logs
        exporters: [google]
`, logPath, debugLogPath)
	if err := agents.SetupOpsAgent(ctx, logger, vm, config); err != nil {
		return err
	}

	// Install Python.
	// TODO: Consider shipping over a prebuilt binary so that we don't need to
	// install Python.
	if gce.IsWindows(vm.Platform) {
		installPython := `$tempDir = "/tmp"
mkdir $tempDir

$pythonUrl = 'https://www.python.org/ftp/python/3.11.2/python-3.11.2.exe'
$pythonInstallerName = $pythonUrl -replace '.*/'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$webClient = New-Object System.Net.WebClient
$webClient.DownloadFile($pythonUrl, "$tempDir\$pythonInstallerName")

$pythonInstallDir = "$env:SystemDrive\Python"
$pythonPath = "$pythonInstallDir\python.exe"
Start-Process "$tempDir\$pythonInstallerName" -Wait -ArgumentList "/quiet TargetDir=$pythonInstallDir InstallAllUsers=1"
`
		if _, err := gce.RunRemotely(ctx, logger, vm, "", installPython); err != nil {
			return fmt.Errorf("Could not install Python: %w", err)
		}
	} else {
		if err := agents.InstallPackages(ctx, logger, vm, []string{"python3"}); err != nil {
			return err
		}
	}
	// Upload log_generator.py.
	if err := gce.UploadContent(ctx, logger, vm, strings.NewReader(logGeneratorSource), logGeneratorPath); err != nil {
		return err
	}

	// Start log_generator.py asynchronously.
	var startLogGenerator string
	if gce.IsWindows(vm.Platform) {
		// The best way I've found to start a process asynchronously. One downside
		// is that standard output and standard error are lost.
		startLogGenerator = fmt.Sprintf(`Invoke-WmiMethod -ComputerName . -Class Win32_Process -Name Create -ArgumentList "$env:SystemDrive\Python\python.exe %v --log-size-in-bytes=%v --log-rate=%v --log-write-type=file --file-path=%v"`, logGeneratorPath, logSizeInBytes, logRate, logPath)
	} else {
		startLogGenerator = fmt.Sprintf(`nohup python3 %v \
  --log-size-in-bytes="%v" \
  --log-rate="%v" \
  --log-write-type=file \
  --file-path="%v" \
  &> %v &
`, logGeneratorPath, logSizeInBytes, logRate, logPath, debugLogPath)
	}
	if _, err := gce.RunRemotely(ctx, logger, vm, "", startLogGenerator); err != nil {
		return err
	}

	// Print log_generator log files to debug startup errors.
	// These log files are unfortunately not available on Windows.
	if !gce.IsWindows(vm.Platform) {
		time.Sleep(5 * time.Second)

		if _, err := gce.RunRemotely(ctx, logger, vm, "", "cat "+debugLogPath); err != nil {
			return err
		}
	}
	return nil
}
