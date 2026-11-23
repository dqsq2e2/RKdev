// SPDX-License-Identifier: GPL-2.0+
/*
 * Key/USB policy derived from the Rockchip and PHX-R3588 U-Boot ports.
 * Hardware wiring and layout are provided by the generated profile.
 */
#include <adc.h>
#include <asm/io.h>
#include <asm/arch-rockchip/boot_mode.h>
#include <blk.h>
#include <button.h>
#include <command.h>
#include <dm.h>
#include <dm/device-internal.h>
#include <dm/lists.h>
#include <dm/uclass.h>
#include <dm/uclass-internal.h>
#include <env.h>
#include <malloc.h>
#include <stdio_dev.h>
#include <time.h>
#include <linux/delay.h>
#include <linux/usb/gadget.h>
#include "rkdev_profile.h"

#if defined(CONFIG_VIDEO) && defined(CONFIG_CMD_BMP) && defined(CONFIG_VIDEO_BMP_GZIP)
#include <mapmem.h>
#include <splash.h>
#include <video.h>
#include <video_console.h>
#include "rkdev_logo.h"

static struct udevice *screen, *screen_console;

static void rkdev_splash(void)
{
	if (!screen) {
		struct udevice *child;
		int ret = uclass_first_device_err(UCLASS_VIDEO, &screen);

		if (ret) {
			screen = NULL;
			if (ret != -ENODEV)
				printf("RKdev: boot display unavailable (%d)\n", ret);
			return;
		}
		device_foreach_child(child, screen) {
			if (device_get_uclass_id(child) == UCLASS_VIDEO_CONSOLE &&
			    device_active(child)) {
				screen_console = child;
				break;
			}
		}
	}
	video_clear(screen);
	if (bmp_display(map_to_sysmem(rkdev_logo), BMP_ALIGN_CENTER, BMP_ALIGN_CENTER))
		puts("RKdev: unable to draw boot logo\n");
}

static void rkdev_screen_line(int row, const char *line)
{
	if (screen_console) {
		vidconsole_set_cursor_pos(screen_console, 32, 24 + row * 16);
		vidconsole_put_string(screen_console, line);
	}
}

static void rkdev_screen_hint(const char *line)
{
	if (screen_console) {
		struct video_priv *v = dev_get_uclass_priv(screen);
		int x = (v->xsize - (int)strlen(line) * 8) / 2;

		vidconsole_set_cursor_pos(screen_console, max(0, x), v->ysize - 32);
		vidconsole_put_string(screen_console, line);
		video_sync(screen, true);
	}
}

static void rkdev_screen_begin(void)
{
	if (screen)
		video_clear(screen);
}

static void rkdev_screen_end(void)
{
	if (screen)
		video_sync(screen, true);
}
#else
static void rkdev_splash(void) { }
static void rkdev_screen_line(int row, const char *line) { }
static void rkdev_screen_hint(const char *line) { }
static void rkdev_screen_begin(void) { }
static void rkdev_screen_end(void) { }
#endif

static enum { NORMAL, RECOVERY, LOADER } request;
static bool layout_env;

#if RKDEV_KEY == 1
static bool key_down(void)
{
	struct udevice *adc;
	unsigned int value;

	if (uclass_get_device_by_driver(UCLASS_ADC,
					DM_DRIVER_GET(rockchip_saradc), &adc))
		return false;
	for (int i = 0; i < RKDEV_KEY_SAMPLES; i++) {
		if (adc_channel_single_shot(adc->name, RKDEV_ADC, &value) ||
		    value < RKDEV_ADC_LOW || value > RKDEV_ADC_HIGH)
			return false;
		if (i + 1 < RKDEV_KEY_SAMPLES)
			mdelay(RKDEV_KEY_DELAY);
	}
	return true;
}
#elif RKDEV_KEY == 2
static bool key_down(void)
{
	struct udevice *dev;

	if (button_get_by_label(RKDEV_BUTTON, &dev))
		return false;
	for (int i = 0; i < RKDEV_KEY_SAMPLES; i++) {
		if (button_get_state(dev) != BUTTON_ON)
			return false;
		if (i + 1 < RKDEV_KEY_SAMPLES)
			mdelay(RKDEV_KEY_DELAY);
	}
	return true;
}
#else
static bool key_down(void)
{
	return false;
}
#endif

#if RKDEV_ACTION == 3
static struct usb_gadget *gadget;
static bool seen_setup;

static int probe_bind(struct usb_gadget *dev)
{
	gadget = dev;
	return 0;
}

static void probe_unbind(struct usb_gadget *dev)
{
	gadget = NULL;
}

static int probe_setup(struct usb_gadget *dev, const struct usb_ctrlrequest *req)
{
	seen_setup = true;
	return -EOPNOTSUPP;
}

static void probe_disconnect(struct usb_gadget *dev)
{
}

static struct usb_gadget_driver probe_driver = {
	.function = "rkdev-host-probe",
	.speed = USB_SPEED_HIGH,
	.bind = probe_bind,
	.unbind = probe_unbind,
	.setup = probe_setup,
	.disconnect = probe_disconnect,
};

static bool host_present(void)
{
	struct udevice *dev;
	ofnode node = ofnode_path(RKDEV_USB_NODE);
	bool temporary = false;
	bool connected = false;
	ulong start;

	if (uclass_find_device_by_ofnode(UCLASS_USB_GADGET_GENERIC, node, &dev)) {
		struct udevice *host;

		/* Leave the host bound so USB storage can still boot normally. */
		if (uclass_find_device_by_ofnode(UCLASS_USB, node, &host) ||
		    device_active(host) ||
		    device_bind_driver_to_node(host->parent, "dwc3-generic-peripheral",
					       "rkdev-otg-probe", node, &dev))
			return false;
		temporary = true;
	}
	if (device_active(dev) || device_probe(dev))
		goto unbind;

	seen_setup = false;
	if (!usb_gadget_register_driver(&probe_driver)) {
		start = get_timer(0);
		while (get_timer(start) < RKDEV_USB_WAIT) {
			if (dm_usb_gadget_handle_interrupts(dev))
				break;
			if (seen_setup || (gadget && gadget->speed != USB_SPEED_UNKNOWN)) {
				connected = true;
				break;
			}
			udelay(1000);
		}
		usb_gadget_unregister_driver(&probe_driver);
	}
	if (device_remove(dev, DM_REMOVE_NORMAL))
		return false;
unbind:
	if (temporary && device_unbind(dev))
		return false;
	return connected;
}
#endif

static int request_maskrom(void)
{
	puts("RKdev: returning to BootROM Maskrom\n");
	/* Preserve the boot-mode flag across an RK3588 PSCI reset with rkbin BL31. */
	writel(0xffff0000, 0xfd58a004);
	writel(BOOT_BROM_DOWNLOAD, CONFIG_ROCKCHIP_BOOT_MODE_REG);
	do_reset(NULL, 0, 0, NULL);
	return CMD_RET_FAILURE;
}

void rkdev_prepare_boot(void)
{
	u32 mode = readl(CONFIG_ROCKCHIP_BOOT_MODE_REG);
	const char *saved_layout = env_get("rkdev_layout");

	layout_env = saved_layout && !strcmp(saved_layout, RKDEV_LAYOUT);
	writel(BOOT_NORMAL, CONFIG_ROCKCHIP_BOOT_MODE_REG);
	env_set("bootcmd", "rkdevboot");
	env_set("preboot", NULL);
	env_set("bootlimit", NULL);
	if (mode == BOOT_RECOVERY) {
		request = RECOVERY;
	} else if (mode == BOOT_LOADER && RKDEV_LOADER[0]) {
		request = LOADER;
	} else if (key_down()) {
#if RKDEV_ACTION == 3
		if (host_present())
			request_maskrom();
#elif RKDEV_ACTION == 2
		request_maskrom();
#endif
		puts("RKdev: recovery key pressed\n");
		request = RECOVERY;
	}
}

static const struct {
	const char *name;
	const char *script;
} targets[] = { RKDEV_TARGETS };

static bool skip_emmc_system(void)
{
	const char *pending = env_get("rkdev_installing");

	return !strcmp(RKDEV_PROFILE, "emmc") &&
		(!layout_env || (pending && *pending && strcmp(pending, "0")));
}

#ifdef CONFIG_USB_KEYBOARD_FN_KEYS
enum { UI_NONE, UI_F2 = 0x100, UI_F10, UI_UP, UI_DOWN, UI_ESCAPE };

static int rkdev_key_from(struct stdio_dev *io, bool serial)
{
	char seq[5] = {0};
	int ch, n = 0;
	ulong start;

	if (!io || !io->tstc || !io->getc || io->tstc(io) <= 0)
		return UI_NONE;
	ch = io->getc(io);
	if (ch != 0x1b)
		return ch == '\r' || ch == '\n' ? '\r' : UI_NONE;

	/* USB queues a whole key sequence; serial bytes can arrive separately. */
	start = get_timer(0);
	while (n < sizeof(seq) - 1) {
		if (io->tstc(io) <= 0) {
			if (!serial || get_timer(start) >= 40)
				break;
			udelay(1000);
			continue;
		}
		seq[n++] = io->getc(io);
		if ((n >= 2 && seq[n - 1] >= 'A' && seq[n - 1] <= 'Z') ||
		    seq[n - 1] == '~')
			break;
	}
	if (!strcmp(seq, "OQ") || !strcmp(seq, "[12~"))
		return UI_F2;
	if (!strcmp(seq, "[21~"))
		return UI_F10;
	if (!strcmp(seq, "[A") || !strcmp(seq, "OA"))
		return UI_UP;
	if (!strcmp(seq, "[B") || !strcmp(seq, "OB"))
		return UI_DOWN;
	return n ? UI_NONE : UI_ESCAPE;
}

static int rkdev_key(void)
{
	struct stdio_dev *io;
	int key = rkdev_key_from(stdio_get_by_name("serial"), true);

	if (key)
		return key;
	list_for_each_entry(io, stdio_get_list(), list) {
		if (strncmp(io->name, "usbkbd", 6))
			continue;
		key = rkdev_key_from(io, false);
		if (key)
			return key;
	}
	return UI_NONE;
}

static const struct rkdev_menu_target {
	const char *name, *bus;
	enum uclass_id cls;
	int devnum;
	const char *probe;
} menu_targets[] = { RKDEV_MENU_TARGETS };

static void rkdev_boot_menu(void)
{
	struct {
		const struct rkdev_menu_target *target;
		int devnum;
		lbaint_t mib;
	} items[24];
	int count = 0, selected = 0;
	bool redraw = true, boot_failed = false;
	ulong start;

	puts("\nRKdev: scanning boot media...\n");
	for (int i = 0; i < ARRAY_SIZE(menu_targets); i++) {
		const struct rkdev_menu_target *t = &menu_targets[i];
		struct udevice *dev;
		int err;

		if (skip_emmc_system() && !strcmp(t->name, "emmc"))
			continue;
		if (run_command(t->probe, 0))
			continue;
		for (err = blk_first_device(t->cls, &dev); !err && count < ARRAY_SIZE(items);
		     err = blk_next_device(&dev)) {
			struct blk_desc *d = dev_get_uclass_plat(dev);

			if (!d || !d->lba || (t->devnum >= 0 && d->devnum != t->devnum))
				continue;
			items[count].target = t;
			items[count].devnum = d->devnum;
			items[count++].mib = (d->lba * d->blksz) >> 20;
		}
	}
	start = get_timer(0);
	while (get_timer(start) < 30000) {
		int key;

		if (redraw) {
			const char title[] = "RKdev one-time boot  [Up/Down, Enter, Esc, F2]";
			char line[72];

			printf("\n%s\n", title);
			rkdev_screen_begin();
			rkdev_screen_line(0, title);
			for (int i = 0; i < count; i++) {
				snprintf(line, sizeof(line), "%c %s %d  (%llu MiB)",
					 i == selected ? '>' : ' ', items[i].target->name,
					 items[i].devnum, (unsigned long long)items[i].mib);
				puts(line);
				putc('\n');
				rkdev_screen_line(i + 2, line);
			}
			printf("%c Continue normal boot\n", selected == count ? '>' : ' ');
			rkdev_screen_line(count + 2, selected == count ?
					  "> Continue normal boot" : "  Continue normal boot");
			if (boot_failed)
				rkdev_screen_line(1, "No usable system on the selected device.");
			rkdev_screen_end();
			redraw = false;
		}
		key = rkdev_key();
		switch (key) {
		case UI_F2:
			request = RECOVERY;
			return;
		case UI_ESCAPE:
			return;
		case UI_UP:
		case UI_DOWN:
			if (count) {
				selected = (selected + (key == UI_UP ? count : 1)) % (count + 1);
				redraw = true;
				boot_failed = false;
				start = get_timer(0);
			}
			break;
		case '\r':
			if (selected == count)
				return;
			if (!run_command(items[selected].target->probe, 0)) {
				env_set("devtype", items[selected].target->bus);
				env_set_ulong("devnum", items[selected].devnum);
				run_command(RKDEV_SCAN_DEVICE, 0);
			}
			puts("No usable system on the selected device.\n");
			boot_failed = true;
			redraw = true;
			start = get_timer(0);
			break;
		}
		schedule();
		udelay(5000);
	}
}

static void rkdev_boot_keys(void)
{
	const char hint[] = "F2 Recovery | F10 Boot Menu";
	ulong start;
	int key;

	/* Only start host controllers after the board-specific download check. */
	run_command("usb start", 0);
	printf("RKdev: %s\n", hint);
	rkdev_screen_hint(hint);
	start = get_timer(0);
	do {
		key = rkdev_key();
		if (key == UI_F2) {
			request = RECOVERY;
			return;
		}
		if (key == UI_F10) {
			rkdev_boot_menu();
			if (IS_ENABLED(CONFIG_VIDEO))
				rkdev_splash();
			return;
		}
		schedule();
		udelay(5000);
	} while (get_timer(start) < 3000);
}
#else
static void rkdev_boot_keys(void) { }
#endif

static void boot_recovery(void)
{
	const char *const names[] = { "kernel_addr_r", "scriptaddr", "fdt_addr_r", "ramdisk_addr_r" };
	const char *const addrs[] = { RKDEV_RECOVERY_KERNEL_ADDR, "0x00c00000", "0x12000000", "0x12180000" };
	char *saved[ARRAY_SIZE(names)];

	puts("RKdev: attempting local recovery\n");
	/* The compressed SPI input must not overlap the bootm output. */
	for (int i = 0; i < ARRAY_SIZE(names); i++) {
		const char *value = env_get(names[i]);

		saved[i] = value ? strdup(value) : NULL;
		env_set(names[i], addrs[i]);
	}
	run_command(RKDEV_RECOVERY, 0);
	for (int i = 0; i < ARRAY_SIZE(names); i++) {
		env_set(names[i], saved[i]);
		free(saved[i]);
	}
}

static bool boot_system(void)
{
	const char *order = layout_env ? env_get("rkdev_targets") : NULL;
	char local[96], *next;
	bool skip_emmc = skip_emmc_system();
	char *cursor;
	unsigned int used = 0;
	bool tried_recovery = false;

	if (!order || !*order || strlen(order) >= sizeof(local))
		order = RKDEV_ORDER;
	strlcpy(local, order, sizeof(local));
	cursor = local;
	while ((next = strsep(&cursor, " "))) {
		if (!strcmp(next, "recovery")) {
			if (!tried_recovery) {
				tried_recovery = true;
				boot_recovery();
			}
			continue;
		}
		for (int i = 0; i < ARRAY_SIZE(targets); i++) {
			if (!(used & BIT(i)) && !strcmp(next, targets[i].name)) {
				used |= BIT(i);
				if (skip_emmc && !strcmp(next, "emmc")) {
					puts("RKdev: skipping unverified eMMC system\n");
					continue;
				}
				printf("RKdev: scanning %s\n", next);
				run_command(targets[i].script, 0);
			}
		}
	}
	return tried_recovery;
}

static int do_rkdevboot(struct cmd_tbl *cmdtp, int flag, int argc, char *const argv[])
{
	bool tried_recovery = false;

	if (argc == 2) {
		if (!strcmp(argv[1], "maskrom"))
			return request_maskrom();
		if (!strcmp(argv[1], "recovery"))
			request = RECOVERY;
		else if (!strcmp(argv[1], "loader") && RKDEV_LOADER[0])
			request = LOADER;
		else
			return CMD_RET_USAGE;
	}
	if (request != LOADER)
		rkdev_splash();
	if (request == NORMAL)
		rkdev_boot_keys();
	if (request == NORMAL)
		tried_recovery = boot_system();
	if (request != LOADER && !tried_recovery)
		boot_recovery();
	if (RKDEV_LOADER[0]) {
		puts("RKdev: RockUSB Loader\n");
		run_command(RKDEV_LOADER, 0);
	}
	if (RKDEV_FALLBACK_MASKROM)
		return request_maskrom();
	request = NORMAL;
	return CMD_RET_FAILURE;
}

U_BOOT_CMD(rkdevboot, 2, 0, do_rkdevboot, "start the RKdev boot policy",
	   "[recovery|maskrom|loader]");
