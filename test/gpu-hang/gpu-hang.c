// Copyright (C) 2026 Intel Corporation
//
// SPDX-License-Identifier: MIT

// gpu-hang puts an xe GPU into a state that needs recovery, for the GPU
// recovery e2e tests.
//
// --mode=wedged (default) follows IGT's xe_wedged@wedged-at-any-timeout: with
// the device's wedged_mode set to 2 ("upon-any-hang-no-reset"), it submits a
// non-preemptible spin batch that never ends and holds the fd open until the
// job times out. xe then declares the device wedged instead of resetting it.
// Unlike the IGT test, it does not rebind the device afterwards: the GPU stays
// wedged until something else (the recovery Job) rebinds it.
//
// --mode=survivability follows IGT's xe_configfs@survivability-mode: unbind
// the device, write 1 to its xe configfs survivability_mode, bind it back. xe
// then probes it into survivability mode, as if the firmware were broken.
// Unlike the IGT test, it leaves the configfs value at 1; the reflash (the
// mock's updatefw -t FDO) clears it.
//
// Exit status: 0 when the device is in the requested state, IGT's failure code
// otherwise. See README.md.

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#include "drmtest.h"
#include "igt_configfs.h"
#include "igt_core.h"
#include "igt_debugfs.h"
#include "igt_kmod.h"
#include "igt_sriov_device.h"
#include "igt_sysfs.h"
#include "igt_syncobj.h"
#include "xe_drm.h"
#include "xe/xe_ioctl.h"
#include "xe/xe_query.h"
#include "xe/xe_spin.h"

#define SPIN_ADDR 0x1a0000

enum mode {
	MODE_WEDGED,
	MODE_SURVIVABILITY,
};

static struct {
	const char *bdf;
	enum mode mode;
	int timeout_s;
	bool set_wedged_mode;
	bool force;
} opt = {
	.timeout_s = 120,
};

static int dev_fd = -1;
static char orig_wedged_mode[16];
static bool wedged_mode_changed;
static bool unbound;

static const char help_str[] =
	"  -b, --bdf=BDF          PCI address of the xe GPU (required)\n"
	"  -m, --mode=MODE        wedged (default) or survivability\n"
	"  -f, --force            Allow using the boot VGA device\n"
	"  wedged mode only:\n"
	"  -t, --timeout=SECONDS  How long to wait for the hang to be detected (default 120)\n"
	"  -w, --set-wedged-mode  Write wedged_mode=2 before the hang. Restored if the\n"
	"                         device does not end up wedged; a rebind resets it anyway\n";

static const struct option long_opts[] = {
	{ "bdf", required_argument, NULL, 'b' },
	{ "mode", required_argument, NULL, 'm' },
	{ "timeout", required_argument, NULL, 't' },
	{ "set-wedged-mode", no_argument, NULL, 'w' },
	{ "force", no_argument, NULL, 'f' },
	{ }
};

static int opt_handler(int o, int idx, void *data)
{
	switch (o) {
	case 'b':
		opt.bdf = optarg;
		break;
	case 'm':
		if (!strcmp(optarg, "wedged"))
			opt.mode = MODE_WEDGED;
		else if (!strcmp(optarg, "survivability"))
			opt.mode = MODE_SURVIVABILITY;
		else
			return IGT_OPT_HANDLER_ERROR;
		break;
	case 't':
		opt.timeout_s = atoi(optarg);
		if (opt.timeout_s <= 0)
			return IGT_OPT_HANDLER_ERROR;
		break;
	case 'w':
		opt.set_wedged_mode = true;
		break;
	case 'f':
		opt.force = true;
		break;
	default:
		return IGT_OPT_HANDLER_ERROR;
	}

	return IGT_OPT_HANDLER_SUCCESS;
}

static bool read_sysfs(const char *path, char *buf, size_t len)
{
	FILE *f = fopen(path, "r");
	bool ok;

	if (!f)
		return false;
	ok = fgets(buf, len, f) != NULL;
	fclose(f);
	if (ok)
		buf[strcspn(buf, "\n")] = '\0';

	return ok;
}

static void check_target(const char *bdf)
{
	char path[PATH_MAX], link[PATH_MAX], buf[16];
	ssize_t n;

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/driver", bdf);
	n = readlink(path, link, sizeof(link) - 1);
	igt_assert_f(n > 0, "%s is not bound to any driver\n", bdf);
	link[n] = '\0';
	igt_assert_f(!strcmp(strrchr(link, '/') + 1, "xe"),
		     "%s is bound to %s, not xe\n", bdf, strrchr(link, '/') + 1);

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/boot_vga", bdf);
	if (read_sysfs(path, buf, sizeof(buf)) && !strcmp(buf, "1"))
		igt_assert_f(opt.force,
			     "%s is the boot VGA device, refusing without --force\n", bdf);
}

static int open_card(const char *bdf)
{
	char path[PATH_MAX];
	struct dirent *de;
	DIR *dir;
	int fd = -1;

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/drm", bdf);
	dir = opendir(path);
	igt_assert_f(dir, "%s has no DRM node: %m\n", bdf);

	while ((de = readdir(dir))) {
		if (strncmp(de->d_name, "card", 4))
			continue;
		snprintf(path, sizeof(path), "/dev/dri/%s", de->d_name);
		fd = open(path, O_RDWR | O_CLOEXEC);
		igt_assert_f(fd >= 0, "cannot open %s: %m\n", path);
		igt_info("Opened %s for %s\n", path, bdf);
		break;
	}
	closedir(dir);
	igt_assert_f(fd >= 0, "%s has no card node\n", bdf);

	return fd;
}

// Mirrors xe_wedged's simple_ioctl: once xe has declared the device wedged,
// ioctls fail with ECANCELED.
static bool device_wedged(int fd)
{
	struct drm_xe_vm_create create = { };

	if (igt_ioctl(fd, DRM_IOCTL_XE_VM_CREATE, &create) == 0) {
		xe_vm_destroy(fd, create.vm_id);
		return false;
	}

	return errno == ECANCELED;
}

static bool read_wedged_mode(int fd, char *buf, size_t len)
{
	if (!igt_debugfs_exists(fd, "wedged_mode", O_RDONLY))
		return false;
	igt_debugfs_read(fd, "wedged_mode", buf);
	buf[strcspn(buf, "\n")] = '\0';

	return buf[0] != '\0';
}

static void restore_wedged_mode(int sig)
{
	if (!wedged_mode_changed || dev_fd < 0 || device_wedged(dev_fd))
		return;

	igt_debugfs_write(dev_fd, "wedged_mode", orig_wedged_mode);
	igt_info("Restored wedged_mode=%s\n", orig_wedged_mode);
}

static void setup_wedged_mode(int fd)
{
	char mode[16];

	igt_require_f(read_wedged_mode(fd, mode, sizeof(mode)),
		      "no wedged_mode in debugfs; the kernel is too old or debugfs is not mounted\n");

	if (opt.set_wedged_mode && strcmp(mode, "2")) {
		snprintf(orig_wedged_mode, sizeof(orig_wedged_mode), "%s", mode);
		igt_install_exit_handler(restore_wedged_mode);
		wedged_mode_changed = true;

		igt_debugfs_write(fd, "wedged_mode", "2");
		igt_assert_f(read_wedged_mode(fd, mode, sizeof(mode)) && !strcmp(mode, "2"),
			     "writing wedged_mode=2 had no effect. It is refused on a VF, "
			     "and on an SR-IOV PF it needs a debug kernel build\n");
		igt_info("Set wedged_mode=2 (was %s)\n", orig_wedged_mode);
	}

	igt_info("wedged_mode=%s\n", mode);
	if (strcmp(mode, "2"))
		igt_warn("wedged_mode is not 2: xe will reset the GT instead of wedging the device. "
			 "Write 2 to it or pass --set-wedged-mode\n");
}

static uint64_t deadline_ns(int seconds)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);

	return (uint64_t)ts.tv_sec * NSEC_PER_SEC + ts.tv_nsec + (uint64_t)seconds * NSEC_PER_SEC;
}

static void hang(int fd)
{
	struct drm_xe_engine_class_instance *eci = &xe_engine(fd, 0)->instance;
	struct drm_xe_sync sync = {
		.type = DRM_XE_SYNC_TYPE_SYNCOBJ,
		.flags = DRM_XE_SYNC_FLAG_SIGNAL,
	};
	struct drm_xe_exec exec = {
		.num_batch_buffer = 1,
		.num_syncs = 1,
		.syncs = to_user_pointer(&sync),
	};
	struct xe_spin_opts spin_opts = { .addr = SPIN_ADDR, .preempt = false };
	struct xe_spin *spin;
	uint32_t vm, bo, queue;
	struct timespec start, end;
	size_t bo_size;
	int i, err;

	vm = xe_vm_create(fd, 0, 0);
	bo_size = xe_bb_size(fd, sizeof(*spin));
	bo = xe_bo_create(fd, vm, bo_size, vram_if_possible(fd, eci->gt_id),
			  DRM_XE_GEM_CREATE_FLAG_NEEDS_VISIBLE_VRAM);
	spin = xe_bo_map(fd, bo, bo_size);
	xe_vm_bind_sync(fd, vm, bo, 0, SPIN_ADDR, bo_size);
	queue = xe_exec_queue_create(fd, vm, eci, 0);

	xe_spin_init(spin, &spin_opts);
	sync.handle = syncobj_create(fd, 0);
	exec.exec_queue_id = queue;
	exec.address = spin_opts.addr;
	xe_exec(fd, &exec);

	for (i = 0; !xe_spin_started(spin); i++) {
		igt_assert_f(i < 10000, "spinner did not start within 10s\n");
		usleep(1000);
	}
	igt_info("Spinner running on %s:%d (gt %d), waiting up to %ds for the hang to be detected\n",
		 xe_engine_class_string(eci->engine_class), eci->engine_instance,
		 eci->gt_id, opt.timeout_s);

	clock_gettime(CLOCK_MONOTONIC, &start);
	// Keep the fd and the queue alive until the timeout fires: tearing the
	// queue down first would cancel the job before it is seen as hung.
	err = syncobj_wait_err(fd, &sync.handle, 1, deadline_ns(opt.timeout_s), 0);
	clock_gettime(CLOCK_MONOTONIC, &end);
	igt_assert_f(err != -ETIME,
		     "no hang detected within %ds; check the engine's job_timeout_ms\n",
		     opt.timeout_s);
	igt_assert_f(err == 0 || err == -ECANCELED, "waiting for the spinner failed: %d\n", err);
	igt_info("Spinner terminated after %.1fs (wait returned %d)\n",
		 igt_time_elapsed(&start, &end), err);

	// The fence can signal just before the device is declared wedged.
	for (i = 0; !device_wedged(fd); i++) {
		igt_assert_f(i < 100, "the hang was handled but the device is not wedged. "
			     "Is wedged_mode 2?\n");
		usleep(100 * 1000);
	}

	// Everything above is left behind on purpose: on a wedged device the
	// destroy ioctls fail, and closing the fd frees it all anyway.
}

// xe adds this attribute to the PCI device when it probes it into survivability
// mode; IGT's xe_configfs test checks for it the same way.
static bool in_survivability(const char *bdf)
{
	char path[PATH_MAX];

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/survivability_mode", bdf);

	return access(path, F_OK) == 0;
}

static void bind_back(int sig)
{
	if (!unbound)
		return;

	if (igt_kmod_bind("xe", opt.bdf) == 0)
		igt_info("Bound %s back to xe\n", opt.bdf);
	else
		igt_warn("Binding %s back to xe failed: %m\n", opt.bdf);
}

static void enter_survivability(const char *bdf)
{
	int fd, cfg_fd, dev_cfg_fd, err, i;
	bool supported, vf, written;

	fd = open_card(bdf);
	xe_device_get(fd);
	supported = xe_has_survivability(fd);
	vf = intel_is_vf_device(fd);
	xe_device_put(fd);
	close(fd);
	igt_require_f(supported, "%s does not support survivability mode: it needs a discrete GPU "
		      "with graphics IP 20.1 or later\n", bdf);
	igt_require_f(!vf, "survivability mode is not supported on a VF\n");

	cfg_fd = igt_configfs_open("xe");
	igt_require_f(cfg_fd >= 0, "no xe configfs: configfs is not mounted or the kernel is too old\n");
	// Configfs groups are created by mkdir and are gone after a reboot.
	igt_assert_f(mkdirat(cfg_fd, bdf, 0755) == 0 || errno == EEXIST,
		     "cannot create configfs group xe/%s: %m\n", bdf);
	dev_cfg_fd = openat(cfg_fd, bdf, O_DIRECTORY);
	igt_assert_f(dev_cfg_fd >= 0, "cannot open configfs group xe/%s: %m\n", bdf);
	close(cfg_fd);

	// xe reads configfs when it probes the device, so the write only counts
	// while the device is unbound.
	igt_install_exit_handler(bind_back);
	err = igt_kmod_unbind("xe", bdf);
	igt_assert_f(err == 0, "unbinding %s from xe failed: %d\n", bdf, err);
	unbound = true;
	igt_info("Unbound %s from xe\n", bdf);

	written = __igt_sysfs_set_boolean(dev_cfg_fd, "survivability_mode", true);
	close(dev_cfg_fd);

	// Bind back before reporting a failed write: a GPU missing from the node
	// is worse than one that did not enter survivability mode.
	err = igt_kmod_bind("xe", bdf);
	igt_assert_f(err == 0, "binding %s back to xe failed: %d\n", bdf, err);
	unbound = false;
	igt_assert_f(written, "writing 1 to configfs xe/%s/survivability_mode failed\n", bdf);
	igt_info("Set configfs xe/%s/survivability_mode=1 and bound %s back to xe\n", bdf, bdf);

	for (i = 0; !in_survivability(bdf); i++) {
		igt_assert_f(i < 100, "%s was rebound but is not in survivability mode\n", bdf);
		usleep(100 * 1000);
	}
}

int igt_simple_main_args("b:m:t:wf", long_opts, help_str, opt_handler, NULL)
{
	igt_assert_f(opt.bdf, "--bdf is required\n");
	check_target(opt.bdf);

	if (in_survivability(opt.bdf)) {
		igt_assert_f(opt.mode == MODE_SURVIVABILITY,
			     "%s is in survivability mode, so it cannot be wedged\n", opt.bdf);
		igt_info("%s is already in survivability mode\n", opt.bdf);
		return;
	}

	if (opt.mode == MODE_SURVIVABILITY) {
		enter_survivability(opt.bdf);
		igt_info("%s is in survivability mode\n", opt.bdf);
		return;
	}

	dev_fd = open_card(opt.bdf);
	xe_device_get(dev_fd);

	if (device_wedged(dev_fd)) {
		igt_info("%s is already wedged\n", opt.bdf);
		return;
	}

	setup_wedged_mode(dev_fd);
	hang(dev_fd);

	igt_info("%s is wedged\n", opt.bdf);
}
