// Copyright (C) 2026 Intel Corporation
//
// SPDX-License-Identifier: MIT

// fake-xpu-smi mimics the subset of xpu-smi commands used by GPU recovery and firmware update
// Jobs.
// For injected/virtual recovery needs, reset-type recovery is simulated by
// re-binding the target PCI device:
//
//  1. write BDF to /sys/bus/pci/drivers/<driver>/unbind
//  2. wait for simulated recovery duration
//  3. write BDF to /sys/bus/pci/drivers/<driver>/bind
//
// A reflash (updatefw -t FDO) does the same, but also writes 0 to
// /sys/kernel/config/xe/<BDF>/survivability_mode while the device is unbound —
// the configfs knob the xe KMD offers for entering survivability mode, which is
// how a test device is put into the state a reflash recovers from. See
// clearSurvivabilityMode. Any other updatefw type is a GPUFirmwareUpdate flash and touches no
// device; see cmdUpdateFW.
//
// The sysfs root is resolved in this order:
//  1. XPU_SMI_SYSFS_ROOT environment variable
//  2. SYSFS_ROOT environment variable
//  3. Default: /sys
//
// Supported subcommands (matching real xpu-smi):
//
//	fake-xpu-smi config   -d <BDF> --reset               (SBR reset)
//	fake-xpu-smi config   -d <BDF> --coldreset           (PCIe slot power cycle)
//	fake-xpu-smi amc      --gpuReset -d <BDF>            (AMC GPU reset)
//	fake-xpu-smi updatefw -d <BDF> -t <type> -f <file>   (firmware update; -d optional for AMC)
//	fake-xpu-smi list                                    (show PCI devices and drivers)

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

const (
	// configfsXeDir is xe's configfs subsystem, relative to the sysfs root. Relative rather than
	// absolute because configfs is mounted under /sys, so an overridden --sysfs-root covers it
	// too and a fake root needs no second knob.
	configfsXeDir = "kernel/config/xe"

	// survivabilityModeAttr is the per-device configfs attribute that makes xe probe the device
	// into survivability mode instead of initialising it normally.
	survivabilityModeAttr = "survivability_mode"

	// fwTypeFDO is the recovery reflash; the only updatefw type that touches the device.
	fwTypeFDO = "FDO"
	// fwTypeAMC is the one type the real tool flashes without a -d.
	fwTypeAMC = "AMC"

	// failMarker in a firmware file's contents makes updatefw fail. An FDO reflash checks it
	// before the rebind, so a failed reflash leaves the device bound.
	failMarker = "FAKE_XPU_SMI_FAIL"

	// updateDelayEnv overrides how long a non-FDO updatefw sleeps, as a Go duration ("3s").
	updateDelayEnv     = "FAKE_XPU_SMI_UPDATE_DELAY"
	defaultUpdateDelay = 3 * time.Second

	reflashDelay = 30 * time.Second
)

// knownFwTypes are the -t values updatefw accepts: the GPUFirmwareUpdate types plus FDO.
var knownFwTypes = []string{
	"GFX", "GFX_DATA", "GFX_CODE_DATA", "GFX_PSCBIN", fwTypeAMC,
	"FAN_TABLE", "VR_CONFIG", "OPROM_CODE", "OPROM_DATA", fwTypeFDO,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 1
	}

	sysfsRoot := resolveSysfsRoot()

	switch args[0] {
	case "config":
		return cmdConfig(sysfsRoot, args[1:])
	case "amc":
		return cmdAMC(sysfsRoot, args[1:])
	case "updatefw":
		return cmdUpdateFW(sysfsRoot, args[1:])
	case "list":
		return cmdList(sysfsRoot, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		printUsage()
		return 1
	}
}

// cmdConfig handles:
//
//	xpu-smi config -d [deviceId] --reset
//	xpu-smi config -d [pciBdfAddress] --coldreset
func cmdConfig(sysfsRoot string, args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	device := fs.String("d", "", "The device ID or PCI BDF address")
	fs.String("device", "", "The device ID or PCI BDF address (long form)")
	doReset := fs.Bool("reset", false, "Reset device by SBR (Secondary Bus Reset)")
	doColdReset := fs.Bool("coldreset", false, "Cold reset device via PCIe slot power cycle")
	fs.Bool("force-reset-gpus", false, "Reset every GPU behind the slot (accepted, ignored)")
	globalSysfsRoot := fs.String("sysfs-root", sysfsRoot, "sysfs root path")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *device == "" {
		fmt.Fprintln(os.Stderr, "config: -d <deviceId|BDF> is required")
		return 1
	}

	if !*doReset && !*doColdReset {
		fmt.Fprintln(os.Stderr, "config: one of --reset or --coldreset must be specified")
		return 1
	}

	resetType := "SBR (Secondary Bus Reset)"
	if *doColdReset {
		resetType = "cold reset (PCIe slot power cycle)"
	}

	fmt.Printf("performing %s on %s via sysfs at %s\n", resetType, *device, *globalSysfsRoot)

	return rebind(*globalSysfsRoot, *device, 7*time.Second)
}

// cmdAMC handles:
//
//	xpu-smi amc --gpuReset -d [deviceId]
//	xpu-smi amc --gpuReset -d [deviceId] -y
func cmdAMC(sysfsRoot string, args []string) int {
	fs := flag.NewFlagSet("amc", flag.ContinueOnError)
	device := fs.String("d", "", "The device ID or PCI BDF address")
	fs.String("device", "", "The device ID or PCI BDF address (long form)")
	gpuReset := fs.Bool("gpuReset", false, "Reset GPU(s) via AMC")
	gpuResetLower := fs.Bool("gpureset", false, "Reset GPU(s) via AMC (the spelling the recovery Job uses)")
	fs.Bool("y", false, "Skip confirmation prompt")
	fs.Bool("yes", false, "Skip confirmation prompt (long form)")
	globalSysfsRoot := fs.String("sysfs-root", sysfsRoot, "sysfs root path")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if !*gpuReset && !*gpuResetLower {
		fmt.Fprintln(os.Stderr, "amc: --gpuReset must be specified")
		return 1
	}

	if *device == "" {
		fmt.Fprintln(os.Stderr, "amc: -d <deviceId|BDF> is required")
		return 1
	}

	fmt.Printf("performing AMC GPU reset on %s via sysfs at %s\n", *device, *globalSysfsRoot)

	return rebind(*globalSysfsRoot, *device, 7*time.Second)
}

// cmdUpdateFW handles:
//
//	xpu-smi updatefw -d [deviceId] -t [type] -f [imageFilePath]
//	xpu-smi updatefw -t AMC -f [imageFilePath]
//
// The type picks one of two behaviours, because the two callers want different things:
//
//   - FDO is the recovery reflash. It sleeps reflashDelay, then rebinds the device, clearing the
//     survivability_mode configfs knob while it is unbound — the rebind is what revives the card in
//     a recovery test. A file containing failMarker fails it before the rebind, leaving the device
//     bound and untouched.
//   - Every other type is a GPUFirmwareUpdate flash. It touches no device at all: it checks the
//     file the fw-copy initContainer staged, sleeps the update delay and reports success. A file
//     whose contents include failMarker makes it exit non-zero instead, which is how an e2e test
//     makes an update Job fail without the CR being able to set anything on the container.
func cmdUpdateFW(sysfsRoot string, args []string) int {
	fs := flag.NewFlagSet("updatefw", flag.ContinueOnError)
	device := fs.String("d", "", "The device ID or PCI BDF address")
	deviceLong := fs.String("device", "", "The device ID or PCI BDF address (long form)")
	fwType := fs.String("t", "", "The firmware type (e.g. GFX, FDO, AMC)")
	fwTypeLong := fs.String("type", "", "The firmware type (long form)")
	file := fs.String("f", "", "The firmware image file path")
	fileLong := fs.String("file", "", "The firmware image file path (long form)")
	fs.Bool("y", false, "Assume yes to all prompts")
	fs.Bool("assumeyes", false, "Assume yes to all prompts (long form)")
	fs.Bool("force", false, "Force firmware update")
	globalSysfsRoot := fs.String("sysfs-root", sysfsRoot, "sysfs root path")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	bdf := firstNonEmpty(*device, *deviceLong)
	typ := firstNonEmpty(*fwType, *fwTypeLong)
	path := firstNonEmpty(*file, *fileLong)

	if typ == "" {
		fmt.Fprintln(os.Stderr, "updatefw: -t <type> is required")
		return 1
	}

	if !slices.Contains(knownFwTypes, typ) {
		fmt.Fprintf(os.Stderr, "updatefw: unknown firmware type %q\n", typ)
		return 1
	}

	// AMC is reached through the auxiliary interface, so the real tool takes no device for it.
	if bdf == "" && typ != fwTypeAMC {
		fmt.Fprintln(os.Stderr, "updatefw: -d <deviceId|BDF> is required")
		return 1
	}

	if path == "" {
		fmt.Fprintln(os.Stderr, "updatefw: -f <imageFilePath> is required")
		return 1
	}

	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "updatefw: cannot read firmware file: %v\n", err)
		return 1
	}

	if typ == fwTypeFDO {
		return reflash(*globalSysfsRoot, bdf, path, content)
	}

	target := bdf
	if target == "" {
		target = "AMC"
	}

	delay := updateDelay()

	fmt.Printf("fake-xpu-smi: simulating %s firmware update on %s with %s (%d bytes, %s) ...\n",
		typ, target, path, len(content), delay)
	time.Sleep(delay)

	if bytes.Contains(content, []byte(failMarker)) {
		fmt.Fprintf(os.Stderr, "fake-xpu-smi: %s firmware update on %s failed: %s found in %s (simulated)\n",
			typ, target, failMarker, path)
		return 1
	}

	fmt.Printf("fake-xpu-smi: updated %s on %s with %s (simulated)\n", typ, target, path)

	return 0
}

// reflash simulates the recovery reflash: wait while the firmware "flashes", then rebind the
// device, clearing survivability mode while it is unbound.
//
// A file containing failMarker fails the flash before the rebind. Failing any later would leave a
// failed recovery that had nonetheless revived the card, which is not something to test against.
func reflash(sysfsRoot, bdf, path string, content []byte) int {
	fmt.Printf("fake-xpu-smi: simulating firmware update (sleeping %s) ...\n", reflashDelay)
	time.Sleep(reflashDelay)

	if bytes.Contains(content, []byte(failMarker)) {
		fmt.Fprintf(os.Stderr, "fake-xpu-smi: FDO firmware update on %s failed: %s found in %s (simulated)\n",
			bdf, failMarker, path)
		return 1
	}

	fmt.Println("fake-xpu-smi: firmware update complete (simulated)")

	return rebindWith(sysfsRoot, bdf, 0, func() error {
		return clearSurvivabilityMode(sysfsRoot, bdf)
	})
}

// updateDelay returns how long a non-FDO update pretends to take. Short by default: update.sh calls
// xpu-smi once per file and per matching GPU, so the delay multiplies quickly.
func updateDelay() time.Duration {
	v := os.Getenv(updateDelayEnv)
	if v == "" {
		return defaultUpdateDelay
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ignoring invalid %s=%q: %v\n", updateDelayEnv, v, err)
		return defaultUpdateDelay
	}

	return d
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// cmdList scans PCI devices from sysfs and prints each BDF with its bound driver.
func cmdList(sysfsRoot string, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	globalSysfsRoot := fs.String("sysfs-root", sysfsRoot, "sysfs root path")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	devicesDir := filepath.Join(*globalSysfsRoot, "bus/pci/devices")
	entries, err := os.ReadDir(devicesDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: cannot read %s: %v\n", devicesDir, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println("(no devices)")
		return 0
	}

	bdfs := make([]string, 0, len(entries))
	for _, entry := range entries {
		bdfs = append(bdfs, entry.Name())
	}
	sort.Strings(bdfs)

	if len(bdfs) == 0 {
		fmt.Println("(no devices)")
		return 0
	}

	fmt.Printf("%-16s  %s\n", "BDF", "DRIVER")
	fmt.Println("-----------------------------------------------")
	for _, bdf := range bdfs {
		driver, err := driverNameForDevice(*globalSysfsRoot, bdf)
		if err != nil {
			driver = "(unbound)"
		}
		fmt.Printf("%-16s  %s\n", bdf, driver)
	}

	return 0
}

func rebind(sysfsRoot, bdf string, delay time.Duration) int {
	return rebindWith(sysfsRoot, bdf, delay, nil)
}

// rebindWith unbinds bdf, runs whileUnbound (if set), waits delay, and binds it back.
//
// The hook runs with the device detached because that is the only window in which writing xe's
// configfs attributes achieves anything: the driver reads them when it probes the device, so a
// write against a bound device changes nothing until something rebinds it.
//
// A hook failure does not abort the rebind. Leaving a GPU unbound because a mock could not write
// a configfs file would be a worse outcome than the problem being reported — the card would
// vanish from the node entirely. The device is bound back first and the failure is reported
// through the exit code afterwards, which is what fails the recovery Job.
func rebindWith(sysfsRoot, bdf string, delay time.Duration, whileUnbound func() error) int {
	driver, err := driverNameForDevice(sysfsRoot, bdf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot resolve bound driver for %s: %v\n", bdf, err)
		return 1
	}

	unbindPath := filepath.Join(sysfsRoot, "bus/pci/drivers", driver, "unbind")
	bindPath := filepath.Join(sysfsRoot, "bus/pci/drivers", driver, "bind")

	if err := writeBDF(unbindPath, bdf); err != nil {
		fmt.Fprintf(os.Stderr, "failed to unbind %s via %s: %v\n", bdf, unbindPath, err)
		return 1
	}

	var hookErr error

	if whileUnbound != nil {
		if hookErr = whileUnbound(); hookErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", hookErr)
		}
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	if err := writeBDF(bindPath, bdf); err != nil {
		fmt.Fprintf(os.Stderr, "failed to bind %s via %s: %v\n", bdf, bindPath, err)
		return 1
	}

	fmt.Printf("rebind complete: %s via driver %s\n", bdf, driver)

	if hookErr != nil {
		return 1
	}

	return 0
}

// clearSurvivabilityMode writes 0 to <sysfsRoot>/kernel/config/xe/<bdf>/survivability_mode,
// creating the device's configfs group first if it is not already there.
//
// This mirrors how a test device is *put* into survivability mode with the xe KMD: unbind the
// device, write 1 to this file, bind it back. So the recovery a reflash simulates writes 0 in the
// same window.
//
// The mkdir is not defensive tidiness. Configfs groups are created by mkdir — that is the
// interface — and they do not survive a reboot, so on a freshly booted node
// /sys/kernel/config/xe exists but has nothing under it and a write with no mkdir first fails
// with ENOENT. A node that has had a device put into survivability mode by hand since boot
// already has the directory, which is exactly why this is easy to miss: it works until the node
// is rebooted.
//
// Writing 0 is not what brings the device back — the rebind is, and per tkatila the value here
// makes no observable difference. It is written anyway so the mock leaves the node in the state a
// real recovery would: a reflash that "succeeds" while leaving a 1 behind is one reboot away from
// a device that is back in survivability mode with nothing to explain it.
func clearSurvivabilityMode(sysfsRoot, bdf string) error {
	dir := filepath.Join(sysfsRoot, configfsXeDir, bdf)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating configfs group %s (is configfs mounted and xe loaded?): %w", dir, err)
	}

	path := filepath.Join(dir, survivabilityModeAttr)

	if err := writeAttr(path, "0"); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf("cleared survivability mode: %s\n", path)

	return nil
}

func driverNameForDevice(sysfsRoot, bdf string) (string, error) {
	driverLink := filepath.Join(sysfsRoot, "bus/pci/devices", bdf, "driver")
	target, err := os.Readlink(driverLink)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", driverLink, err)
	}

	return filepath.Base(target), nil
}

func writeBDF(path, bdf string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.WriteString(bdf); err != nil {
		return err
	}

	return nil
}

// writeAttr writes value to a configfs attribute, with the same open flags `echo x > file` uses.
//
// O_CREATE is what separates this from writeBDF, and it is there for the fake sysfs root: under a
// real configfs the attribute already exists after the group's mkdir and the flag is ignored,
// while under a temp directory standing in for /sys nothing creates it. Opening without O_CREATE
// would make the mock work on hardware and fail in a unit test, which is the wrong way round.
func writeAttr(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.WriteString(value); err != nil {
		return err
	}

	return nil
}

// resolveSysfsRoot returns the sysfs root from env or default.
func resolveSysfsRoot() string {
	if v := os.Getenv("XPU_SMI_SYSFS_ROOT"); v != "" {
		return v
	}
	if v := os.Getenv("SYSFS_ROOT"); v != "" {
		return v
	}

	return "/sys"
}

func printUsage() {
	usage := `fake-xpu-smi - mock xpu-smi tool for GPU recovery testing

Usage:
  fake-xpu-smi config   -d <deviceId|BDF> --reset                      SBR reset
  fake-xpu-smi config   -d <pciBdfAddress> --coldreset                  PCIe slot power cycle
  fake-xpu-smi amc      --gpuReset -d <deviceId|BDF> [-y]              AMC GPU reset
  fake-xpu-smi updatefw -d <deviceId|BDF> -t <type> -f <imageFilePath>  Firmware update (-d optional for AMC)
  fake-xpu-smi list                                                     List devices

All subcommands accept --sysfs-root <path> to override the sysfs root.
Environment variable precedence:
  1. XPU_SMI_SYSFS_ROOT
  2. SYSFS_ROOT
  3. /sys (default)

All recovery commands (config, amc, updatefw -t FDO) perform:
  unbind + bind of the provided BDF in /sys/bus/pci/drivers/<driver>/...

Any other updatefw type touches no device: it checks the file, sleeps
$FAKE_XPU_SMI_UPDATE_DELAY (default 3s) and succeeds, or fails if the file
contains FAKE_XPU_SMI_FAIL.

updatefw -t FDO additionally clears xe's survivability mode while the device is unbound:
  mkdir  <sysfs-root>/kernel/config/xe/<BDF>          (configfs group; gone after a reboot)
  write 0 to that directory's survivability_mode file`
	fmt.Fprintln(os.Stderr, usage)
}
