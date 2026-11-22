// SPDX-License-Identifier: GPL-2.0+
/*
 * Key/USB policy derived from the Rockchip and PHX-R3588 U-Boot ports.
 * Hardware wiring and layout are provided by the generated profile.
 */
#include <adc.h>
#include <asm/io.h>
#include <asm/arch-rockchip/boot_mode.h>
#include <button.h>
#include <command.h>
#include <dm.h>
#include <dm/device-internal.h>
#include <dm/lists.h>
#include <dm/uclass.h>
#include <dm/uclass-internal.h>
#include <env.h>
#include <malloc.h>
#include <time.h>
#include <linux/delay.h>
#include <linux/usb/gadget.h>
#include "rkdev_profile.h"

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
	const char *pending = env_get("rkdev_installing");
	bool skip_emmc = !strcmp(RKDEV_PROFILE, "emmc") &&
		(!layout_env || (pending && *pending && strcmp(pending, "0")));
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
