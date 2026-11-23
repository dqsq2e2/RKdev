// SPDX-License-Identifier: GPL-2.0+
/*
 * RK3588 boot framebuffer: VP0, a linear XRGB8888 plane and one HDMI QP
 * transmitter. Based on the display, PMU and HDPTX sequences in Linux
 * (drivers/gpu/drm/{rockchip,bridge/synopsys}, drivers/pmdomain/rockchip and
 * drivers/phy/rockchip).
 *
 * Copyright (c) 2021-2022 Rockchip Electronics Co., Ltd.
 * Copyright (c) 2024 Collabora Ltd.
 */

#include <clk.h>
#include <dm.h>
#include <dm/ofnode.h>
#include <dm/uclass.h>
#include <linux/build_bug.h>
#include <malloc.h>
#include <mapmem.h>
#include <asm/arch-rockchip/cru_rk3588.h>
#include <asm/cache.h>
#include <asm/io.h>
#include <dt-bindings/clock/rockchip,rk3588-cru.h>
#include <linux/delay.h>
#include <linux/iopoll.h>
#include <video.h>
#include "rk3588-hdptx.h"

/* PMU bus-idle and memory repair controls */
#define PMU_IDLE_REQ		0x10c
#define PMU_IDLE_ACK		0x118
#define PMU_IDLE_STATUS		0x120
#define PMU_PWR_GATE		0x150
#define PMU_MEM_GATE		0x1a4
#define PMU_CHAIN_STATUS	0x1f0
#define PMU_MEM_STATUS		0x1f8
#define PMU_BISR_CON3		0x20c
#define PMU_REPAIR_STATUS	0x290
#define PMU_BISR_STATUS5	0x294
#define PMU_ESMART_REPAIR	BIT(15)

/* VOP2 global, video port 0 and Esmart0 register offsets */
#define VOP_CFG_DONE		0x000
#define VOP_VERSION		0x004
#define VOP_AUTO_GATING		0x008
#define VOP_IF_EN		0x028
#define VOP_IF_DIV		0x02c
#define VOP_IF_POL		0x030
#define VOP_PD_CTRL		0x034
#define VOP_SYS_STATUS0		0x060
#define VOP_VP0_INT_CLR		0x0a4
#define VOP_VP0_INT_RAW		0x0ac
#define VOP_OVL_CTRL		0x600
#define VOP_LAYER_SEL		0x604
#define VOP_PORT_SEL		0x608
#define VOP_VP0_BG_DELAY	0x6e0
#define VOP_ESMART_DLY		0x6f8
#define VP0_DSP_CTRL		0xc00
#define VP0_CLK_CTRL		0xc0c
#define VP0_DSP_BG		0xc2c
#define VP0_PRE_SCAN		0xc30
#define VP0_POST_HACT		0xc34
#define VP0_POST_VACT		0xc38
#define VP0_POST_SCALE_CTRL	0xc40
#define VP0_HTOTAL		0xc48
#define VP0_HACT		0xc4c
#define VP0_VTOTAL		0xc50
#define VP0_VACT		0xc54
#define VP0_BCSH_CTRL		0xc60
#define ES0_CTRL0		0x1800
#define ES0_CTRL1		0x1804
#define ES0_REGION0_CTRL	0x1810
#define ES0_YRGB_ADDR		0x1814
#define ES0_VIR			0x181c
#define ES0_ACT			0x1820
#define ES0_DSP			0x1824
#define ES0_DSP_ST		0x1828
#define ES0_SCL_CTRL		0x1830

#define VOP_GLOBAL_CFG_DONE	BIT(15)
#define VOP_VP0_CFG_DONE	(BIT(16) | BIT(0))
#define VOP_VP0_FRAME_START	BIT(0)
#define VOP_VP0_HOLD_VALID	BIT(6)
#define VOP_VP_STANDBY		BIT(31)
#define VOP_ESMART_OFF		BIT(7)
#define VOP_ESMART_STATUS	BIT(15)

/* HDMI QP and HDMI PHY register offsets */
#define QP_TIMER_BASE		0x080
#define QP_CMU_STATUS		0x0b0
#define QP_DDC_SS_SCL		0x0e0
#define QP_DDC_FS_SCL		0x0e4
#define QP_DDC_RESET		0x0ec
#define QP_DDC_CTRL		0x0f4
#define QP_DDC_DATA		0x10c
#define QP_HDCP2_CTRL		0x8e0
#define QP_LINK_CTRL		0x968
#define QP_PKT_CFG1		0xa9c
#define QP_PKT_EN		0xaa8
#define QP_AVI_HDR		0xbe0
#define QP_AVI_DATA		0xbe4
#define QP_INT0_MASK		0x3014
#define QP_INT1_STATUS		0x3020
#define QP_INT1_MASK		0x3024
#define QP_INT1_CLEAR		0x3028
#define QP_DDC_DONE		BIT(0)
#define QP_DDC_NACK		BIT(2)
#define QP_DDC_READ		BIT(2)
#define QP_DISPLAY_CLOCKS	GENMASK(5, 0)
#define QP_DISPLAY_LOCKED	0x15
#define QP_DVI_MODE		BIT(4)
#define QP_AVI_ENABLE		BIT(13)
#define QP_GCP_ENABLE		BIT(3)
#define PHY_PLL_EN		BIT(7)
#define PHY_BIAS_EN		BIT(6)
#define PHY_BG_EN		BIT(5)
#define PHY_LOCK		BIT(3)
#define PHY_CLK_READY		BIT(2)
#define PHY_LANE_READY		BIT(1)

/* RK3588 DT reset IDs are sparse; the CRU uses these register/bit pairs. */
#define CRU_RESET(reg, bit)	((RK3588_SOFTRST_CON(reg) << 4) | (bit))
#define PMU_RESET(reg, bit)	((RK3588_PMU_SOFTRST_CON(reg) << 4) | (bit))
#define RST_VOP_HCLK		CRU_RESET(52, 8)
#define RST_VOP_ACLK		CRU_RESET(52, 9)
#define RST_VOP_DCLK0		CRU_RESET(52, 13)

static const struct rk3588_hdmi_reset {
	unsigned int pclk, ref, hpd, bridge;
	unsigned int phy_pclk, phy, ropll, init, common, lane;
} hdmi_reset[] = {
	{ CRU_RESET(60, 11), CRU_RESET(61, 0), CRU_RESET(73, 12), CRU_RESET(52, 14),
	  CRU_RESET(72, 5), CRU_RESET(72, 14), CRU_RESET(72, 12),
	  PMU_RESET(3, 11), PMU_RESET(3, 12), PMU_RESET(3, 13) },
	{ CRU_RESET(61, 2), CRU_RESET(61, 7), CRU_RESET(73, 13), CRU_RESET(52, 15),
	  CRU_RESET(72, 6), CRU_RESET(73, 1), CRU_RESET(72, 15),
	  PMU_RESET(3, 15), PMU_RESET(4, 0), PMU_RESET(4, 1) },
};

struct rk3588_hdmi_mode {
	u32 pixel_hz;
	u16 width, hfront, hsync, hback;
	u16 height, vfront, vsync, vback;
	u8 vic;
	bool negative_sync;
	/* HDPTX ROPLL values for 24 MHz reference and 8 bits per component. */
	u8 mdiv, sdiv, sdm_deno, sdm_num, sdm_negative, sdc_n, sdc_num, sdc_deno;
};

static const struct rk3588_hdmi_mode modes[] = {
	{ 148500000, 1920, 88, 44, 148, 1080, 4, 5, 36, 16, false,
	  123, 3, 4, 3, 0, 5, 5, 16 },
	{ 74250000, 1280, 110, 40, 220, 720, 5, 5, 20, 4, false,
	  124, 7, 62, 16, 1, 5, 0, 1 },
	{ 25175000, 640, 16, 96, 48, 480, 10, 2, 33, 1, true,
	  84, 15, 168, 16, 1, 4, 1, 1 },
};

struct rk3588_boot_video {
	void __iomem *vop, *pmu, *grf, *vop_grf, *vo_grf;
	void __iomem *qp, *phy, *phy_grf;
	void *fb;
	unsigned int port;
	bool scanout, phy_started;
};

static void __iomem *node_regs(ofnode node)
{
	fdt_addr_t addr = ofnode_valid(node) ? ofnode_get_addr(node) : FDT_ADDR_T_NONE;

	return addr == FDT_ADDR_T_NONE ? NULL : (void __iomem *)(uintptr_t)addr;
}

static void __iomem *phandle_regs(ofnode node, const char *name)
{
	return node_regs(ofnode_parse_phandle(node, name, 0));
}

static void hiword(void __iomem *reg, u32 mask, u32 value)
{
	writel(mask << 16 | (value & mask), reg);
}

static void gate_enable(unsigned int reg, u32 mask)
{
	hiword((void __iomem *)CRU_BASE + RK3588_CLKGATE_CON(reg), mask, 0);
}

static void reset_set(unsigned int reset, bool assert)
{
	u32 mask = BIT(reset & 15);

	hiword((void __iomem *)CRU_BASE + (reset >> 4), mask, assert ? mask : 0);
}

static void reset_pulse(unsigned int id)
{
	reset_set(id, true);
	udelay(20);
	reset_set(id, false);
}

static int rk3588_domain_on(void __iomem *pmu, u32 pwr, u32 mem,
			   u32 repair, unsigned int req_reg, u32 req, u32 idle)
{
	u32 val;
	int ret;

	if ((readl(pmu + PMU_REPAIR_STATUS) & repair) != repair) {
		bool mem_on = !(readl(pmu + PMU_MEM_STATUS) & mem);

		hiword(pmu + PMU_PWR_GATE, pwr, 0);
		if (mem_on) {
			ret = readl_poll_timeout(pmu + PMU_CHAIN_STATUS, val,
						 val & mem, 10000);
			if (ret)
				return ret;
			udelay(20);
			hiword(pmu + PMU_MEM_GATE, pwr, pwr);
			ret = readl_poll_timeout(pmu + PMU_MEM_STATUS, val,
						 val & mem, 10000);
			hiword(pmu + PMU_MEM_GATE, pwr, 0);
			if (ret)
				return ret;
			ret = readl_poll_timeout(pmu + PMU_MEM_STATUS, val,
						 !(val & mem), 10000);
			if (ret)
				return ret;
		}
		ret = readl_poll_timeout(pmu + PMU_REPAIR_STATUS, val,
					 val & repair, 10000);
		if (ret)
			return ret;
	}
	hiword(pmu + PMU_IDLE_REQ + req_reg, req, 0);
	ret = readl_poll_timeout(pmu + PMU_IDLE_ACK, val, !(val & idle), 10000);
	if (ret)
		return ret;
	return readl_poll_timeout(pmu + PMU_IDLE_STATUS, val, !(val & idle), 10000);
}

static int rk3588_display_clocks(struct udevice *dev,
				 struct rk3588_boot_video *p, ulong *ref)
{
	void __iomem *cru = (void __iomem *)CRU_BASE;
	struct clk bus, gpll;
	ulong rate;
	int ret;

	ret = clk_get_by_name(dev, "aclk", &bus);
	if (ret)
		return ret;
	gpll = (struct clk){ .dev = bus.dev, .id = PLL_GPLL };
	rate = clk_get_rate(&gpll);
	if (IS_ERR_VALUE(rate) || !rate)
		return -EINVAL;
	/* ACLK_VOP = GPLL / 2. Keep the HDMI QP reference on GPLL / 4. */
	hiword(cru + RK3588_CLKSEL_CON(110), GENMASK(13, 0), 1);
	hiword(cru + RK3588_CLKSEL_CON(115), BIT(9), 0);
	hiword(cru + RK3588_CLKSEL_CON(128), GENMASK(6, 0), 3);
	*ref = rate / 4;

	gate_enable(52, GENMASK(3, 0) | BIT(8) | BIT(9) | BIT(13));
	gate_enable(59, GENMASK(6, 0) | BIT(9) | BIT(12));
	gate_enable(60, BIT(5) | BIT(6));
	gate_enable(74, BIT(0) | BIT(2));
	ret = rk3588_domain_on(p->pmu, BIT(1), BIT(25), BIT(16), 0,
			       BIT(13) | BIT(14), BIT(13) | BIT(14));
	if (ret)
		return ret;
	return rk3588_domain_on(p->pmu, BIT(3), BIT(27), BIT(18), 4,
			       BIT(0), BIT(16));
}

static int qp_find_sink(struct udevice *video, struct rk3588_boot_video *p)
{
	ofnode node = dev_ofnode(video);

	for (int i = 0; i < 2; i++) {
		ofnode hdmi = ofnode_parse_phandle(node, "u-boot,hdmi", i);
		struct udevice *tx;
		void __iomem *addr;
		unsigned int port;

		if (!ofnode_valid(hdmi) || !ofnode_is_enabled(hdmi) ||
		    !ofnode_device_is_compatible(hdmi, "rockchip,rk3588-dw-hdmi-qp"))
			continue;
		addr = node_regs(hdmi);
		if (addr == (void __iomem *)0xfde80000)
			port = 0;
		else if (addr == (void __iomem *)0xfdea0000)
			port = 1;
		else
			continue;

		/* Probe selects the connector's default pin mux. */
		if (uclass_get_device_by_ofnode(UCLASS_DISPLAY, hdmi, &tx))
			continue;
		gate_enable(73, BIT(12 + port));
		hiword(p->grf + 0x31c, BIT(12 + port), BIT(12 + port));
		/* The HPD pad is outside VO1 and can be checked headless. */
		udelay(1000);
		if (!(readl(p->grf + 0x384) & BIT(port ? 24 : 16)))
			continue;
		p->qp = addr;
		p->port = port;
		p->phy = phandle_regs(hdmi, "phys");
		p->phy_grf = phandle_regs(ofnode_parse_phandle(hdmi, "phys", 0),
					  "rockchip,grf");
		return p->phy && p->phy_grf ? 0 : -EINVAL;
	}
	return -ENODEV;
}

static void qp_init(struct rk3588_boot_video *p, ulong ref)
{
	const struct rk3588_hdmi_reset *rst = &hdmi_reset[p->port];
	u32 io = BIT(9) | BIT(10) | BIT(11) | BIT(13);
	u32 grant = BIT(p->port ? 12 : 10);
	u32 scl = DIV_ROUND_UP(ref, 200000);

	gate_enable(72, BIT(5 + p->port));
	gate_enable(73, BIT(12 + p->port));
	if (!p->port)
		gate_enable(60, BIT(11) | BIT(15));
	gate_enable(61, p->port ? BIT(2) | BIT(6) | BIT(7) : BIT(0));
	reset_pulse(rst->pclk);
	reset_set(rst->ref, false);
	reset_set(rst->hpd, false);
	hiword(p->vo_grf + (p->port ? 0x18 : 0x0c), io | GENMASK(7, 0), io);
	hiword(p->vo_grf + 0x24, grant, grant);
	hiword(p->grf + 0x308, BIT(13 + p->port * 2), BIT(13 + p->port * 2));

	writel(0, p->qp + QP_INT0_MASK);
	writel(0, p->qp + QP_INT1_MASK);
	writel(ref, p->qp + QP_TIMER_BASE);
	writel(1, p->qp + QP_DDC_RESET);
	writel(scl << 16 | scl, p->qp + QP_DDC_SS_SCL);
	writel(scl << 16 | scl, p->qp + QP_DDC_FS_SCL);
}

static int qp_read_edid(struct rk3588_boot_video *p, u8 data[128], unsigned int base)
{
	u32 mask = QP_DDC_NACK | QP_DDC_DONE;
	u32 status;
	unsigned int sum = 0;
	int ret = 0;

	writel(mask, p->qp + QP_INT1_MASK);
	for (unsigned int i = 0; i < 128; i++) {
		u32 cmd = ((base + i) << 12) | (0x50 << 5);

		writel(mask, p->qp + QP_INT1_CLEAR);
		writel(cmd, p->qp + QP_DDC_CTRL);
		writel(cmd | QP_DDC_READ, p->qp + QP_DDC_CTRL);
		ret = readl_poll_timeout(p->qp + QP_INT1_STATUS, status,
					 status & mask, 2000);
		if (ret || (status & QP_DDC_NACK)) {
			writel(1, p->qp + QP_DDC_RESET);
			ret = -EIO;
			break;
		}
		data[i] = readl(p->qp + QP_DDC_DATA);
		sum += data[i];
	}
	writel(0, p->qp + QP_DDC_CTRL);
	writel(mask, p->qp + QP_INT1_CLEAR);
	writel(0, p->qp + QP_INT1_MASK);
	return ret ? ret : (sum & 0xff) ? -EINVAL : 0;
}

static unsigned int edid_modes(const u8 *edid, unsigned int start, unsigned int end)
{
	unsigned int mask = 0;

	for (unsigned int i = start; i + 18 <= end; i += 18) {
		const u8 *d = edid + i;
		u32 hz = ((d[1] << 8) | d[0]) * 10000;
		u16 width = ((d[4] & 0xf0) << 4) | d[2];
		u16 height = ((d[7] & 0xf0) << 4) | d[5];
		u16 hblank = ((d[4] & 0x0f) << 8) | d[3];
		u16 vblank = ((d[7] & 0x0f) << 8) | d[6];

		if (!hz || (d[17] & BIT(7)))
			continue;
		for (unsigned int j = 0; j < ARRAY_SIZE(modes); j++) {
			const struct rk3588_hdmi_mode *m = &modes[j];

			if (hz >= m->pixel_hz - 5000 && hz <= m->pixel_hz + 5000 &&
			    width == m->width && height == m->height &&
			    hblank == m->hfront + m->hsync + m->hback &&
			    vblank == m->vfront + m->vsync + m->vback)
				mask |= BIT(j);
		}
	}
	return mask;
}

static const struct rk3588_hdmi_mode *qp_select_mode(struct rk3588_boot_video *p, bool *hdmi)
{
	static const u8 header[] = { 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0 };
	unsigned int mask = 0;
	u8 data[128];

	if (qp_read_edid(p, data, 0) || memcmp(data, header, sizeof(header)))
		return &modes[1]; /* DDC unavailable: use 720p for a connected sink. */
	mask = edid_modes(data, 54, 126);
	if (data[35] & BIT(5))
		mask |= BIT(2); /* Standard 640x480 at 60 Hz. */

	if (data[126] && !qp_read_edid(p, data, 128) && data[0] == 0x02 &&
	    data[2] >= 4 && data[2] <= 127) {
		mask |= edid_modes(data, data[2], 127);
		for (unsigned int i = 4; i < data[2]; ) {
			unsigned int tag = data[i] >> 5, len = data[i] & 0x1f;

			if (i + len + 1 > data[2])
				break;
			if (tag == 3 && len >= 3 && data[i + 1] == 0x03 &&
			    data[i + 2] == 0x0c && data[i + 3] == 0)
				*hdmi = true;
			if (tag == 2) {
				for (unsigned int k = 1; k <= len; k++)
					for (unsigned int j = 0; j < ARRAY_SIZE(modes); j++)
						if ((data[i + k] & 0x7f) == modes[j].vic)
							mask |= BIT(j);
			}
			i += len + 1;
		}
	}
	for (unsigned int i = 0; i < ARRAY_SIZE(modes); i++)
		if (mask & BIT(i))
			return &modes[i];
	return &modes[1];
}

static void phy_write_table(void __iomem *phy, const struct hdptx_reg *table,
			    unsigned int count, unsigned int base)
{
	for (unsigned int i = 0; i < count; i++)
		writel(table[i].value, phy + (base + table[i].reg) * 4);
}

#define PHY_SEQ(phy, table, base) phy_write_table(phy, table, ARRAY_SIZE(table), base)

static int rk3588_hdptx_enable(struct rk3588_boot_video *p, const struct rk3588_hdmi_mode *m)
{
	const struct rk3588_hdmi_reset *rst = &hdmi_reset[p->port];
	void __iomem *phy = p->phy;
	u32 val;
	int ret;

	p->phy_started = true;
	reset_set(rst->phy, false);
	reset_set(rst->ropll, false);
	reset_pulse(rst->phy_pclk);
	reset_set(rst->lane, true);
	reset_set(rst->common, true);
	reset_set(rst->init, true);
	hiword(p->phy_grf, PHY_PLL_EN | PHY_BIAS_EN | PHY_BG_EN | BIT(11), 0);
	PHY_SEQ(phy, common_cmn_init_seq, 0);
	PHY_SEQ(phy, tmds_cmn_init_seq, 0);
	writel(m->mdiv, phy + 0x51 * 4);
	writel(m->mdiv, phy + 0x55 * 4);
	writel(0x11, phy + 0x59 * 4);
	writel(m->sdiv << 4, phy + 0x5a * 4);
	setbits_le32(phy + 0x5e * 4, BIT(6)); /* Fractional SDM enable */
	writel(m->sdm_deno, phy + 0x60 * 4);
	clrsetbits_le32(phy + 0x64 * 4, BIT(3), m->sdm_negative << 3);
	writel(m->sdm_num, phy + 0x65 * 4);
	clrsetbits_le32(phy + 0x69 * 4, GENMASK(2, 0), m->sdc_n);
	writel(m->sdc_num, phy + 0x6c * 4);
	writel(m->sdc_deno, phy + 0x70 * 4);
	writel(m->sdiv << 4 | BIT(0), phy + 0x86 * 4); /* Pixel-clock output, 8 bpc */

	hiword(p->phy_grf, PHY_BIAS_EN | PHY_BG_EN, PHY_BIAS_EN | PHY_BG_EN);
	udelay(15);
	reset_set(rst->init, false);
	udelay(15);
	hiword(p->phy_grf, PHY_PLL_EN, PHY_PLL_EN);
	udelay(15);
	reset_set(rst->common, false);
	ret = readl_poll_timeout(p->phy_grf + 0x80, val, val & PHY_CLK_READY, 1000);
	if (ret)
		return ret;
	PHY_SEQ(phy, common_sb_init_seq, 0);
	writel(0x06, phy + 0x200 * 4); /* TMDS mode */
	PHY_SEQ(phy, tmds_lntop_lowbr_seq, 0);
	writel(0x07, phy + 0x206 * 4);
	for (unsigned int i = 3; i <= 6; i++) {
		PHY_SEQ(phy, common_lane_init_seq, i * 0x100);
		PHY_SEQ(phy, tmds_lane_init_seq, i * 0x100);
	}
	writel(0x0a, phy + 0x61e * 4); /* Clock-lane skew adjustment */
	reset_set(rst->lane, false);
	writel(0x0f, phy + 0x207 * 4);
	return readl_poll_timeout(p->phy_grf + 0x80, val,
				  (val & (PHY_LOCK | PHY_LANE_READY)) ==
				  (PHY_LOCK | PHY_LANE_READY), 5000);
}

static int qp_enable_video(struct rk3588_boot_video *p, const struct rk3588_hdmi_mode *m,
			   bool hdmi)
{
	u8 aspect = (m->width == 640 ? 0x10 : 0x20) | 0x08;
	u8 checksum = -(0x82 + 2 + 13 + 0x10 + aspect + 0x08 + m->vic);
	u32 val;
	int ret;

	/* Packet scheduler registers are inaccessible without the video clocks. */
	ret = readl_poll_timeout(p->qp + QP_CMU_STATUS, val,
				 (val & QP_DISPLAY_CLOCKS) == QP_DISPLAY_LOCKED, 50000);
	if (ret)
		return ret;
	setbits_le32(p->qp + QP_HDCP2_CTRL, BIT(0)); /* Unencrypted boot output */
	writel(hdmi ? 0 : QP_DVI_MODE, p->qp + QP_LINK_CTRL);
	if (!hdmi)
		return 0;

	/* AVI: RGB full range, active picture matches the coded aspect ratio. */
	writel(2 << 8 | 13 << 16, p->qp + QP_AVI_HDR);
	writel(checksum | 0x10 << 8 | aspect << 16 | 0x08 << 24, p->qp + QP_AVI_DATA);
	writel(m->vic, p->qp + QP_AVI_DATA + 4);
	writel(0, p->qp + QP_AVI_DATA + 8);
	writel(0, p->qp + QP_AVI_DATA + 12);
	clrbits_le32(p->qp + QP_PKT_CFG1, BIT(12));
	writel(QP_AVI_ENABLE | QP_GCP_ENABLE, p->qp + QP_PKT_EN);
	return 0;
}

static void vop_commit(struct rk3588_boot_video *p)
{
	writel(VOP_GLOBAL_CFG_DONE | VOP_VP0_CFG_DONE, p->vop + VOP_CFG_DONE);
}

static int vop_esmart_on(struct rk3588_boot_video *p)
{
	u32 val;

	clrbits_le32(p->vop + VOP_PD_CTRL, VOP_ESMART_OFF);
	/* With memory repair enabled, the PMU owns the power status. */
	if (readl(p->pmu + PMU_BISR_CON3) & PMU_ESMART_REPAIR)
		return readl_poll_timeout(p->pmu + PMU_BISR_STATUS5, val,
					  val & PMU_ESMART_REPAIR, 50000);
	return readl_poll_timeout(p->vop + VOP_SYS_STATUS0, val,
				  !(val & VOP_ESMART_STATUS), 50000);
}

static int vop_enable(struct rk3588_boot_video *p, const struct rk3588_hdmi_mode *m,
		      ulong framebuffer)
{
	u16 hstart = m->hsync + m->hback, vstart = m->vsync + m->vback;
	u16 htotal = hstart + m->width + m->hfront, vtotal = vstart + m->height + m->vfront;
	u32 size = (m->height - 1) << 16 | (m->width - 1);
	u32 hact = hstart << 16 | (hstart + m->width);
	u32 vact = vstart << 16 | (vstart + m->height);
	u32 pol = 5 + p->port * 2;
	u32 val;
	int ret;

	/* DCLK_VOP0 mux inputs 1 and 2 are the HDMI0/1 PHY pixel clocks. */
	hiword((void __iomem *)CRU_BASE + RK3588_CLKSEL_CON(112),
	       GENMASK(8, 7), (p->port + 1) << 7);
	reset_pulse(RST_VOP_HCLK);
	reset_pulse(RST_VOP_ACLK);
	reset_pulse(RST_VOP_DCLK0);
	reset_pulse(hdmi_reset[p->port].bridge);
	if (readl(p->vop + VOP_VERSION) != 0x40176786)
		return -ENODEV;
	writel(VOP_VP_STANDBY, p->vop + VP0_DSP_CTRL);
	writel(VOP_GLOBAL_CFG_DONE, p->vop + VOP_CFG_DONE);
	clrbits_le32(p->vop + VOP_AUTO_GATING, BIT(31));
	ret = vop_esmart_on(p);
	if (ret)
		return ret;

	writel(2, p->vop + VP0_CLK_CTRL); /* VP core runs at pixel clock / 4. */
	writel(BIT(3 + p->port * 2), p->vop + VOP_IF_EN); /* HDMI via VP0 */
	writel(6 << (16 + p->port * 4), p->vop + VOP_IF_DIV);
	writel(BIT(28), p->vop + VOP_IF_POL); /* Apply the interface mux immediately. */
	hiword(p->vop_grf + 8, BIT(1 + p->port * 3), BIT(1 + p->port * 3));
	hiword(p->vo_grf, 3 << pol, m->negative_sync ? 3 << pol : 0);

	writel(0, p->vop + VOP_OVL_CTRL); /* RGB layer mixer */
	writel(0x76543012, p->vop + VOP_LAYER_SEL); /* Esmart0 is layer 0. */
	writel(0x7880, p->vop + VOP_PORT_SEL); /* VP0 uses only layer 0. */
	writel(23, p->vop + VOP_ESMART_DLY);
	writel(54 << 24, p->vop + VOP_VP0_BG_DELAY);
	writel((54 + (m->width >> 1) - 1) << 16 | m->hsync, p->vop + VP0_PRE_SCAN);
	writel(0, p->vop + VP0_DSP_BG);
	writel(0, p->vop + VP0_BCSH_CTRL);
	writel(htotal << 16 | m->hsync, p->vop + VP0_HTOTAL);
	writel(vtotal << 16 | m->vsync, p->vop + VP0_VTOTAL);
	writel(hact, p->vop + VP0_HACT);
	writel(vact, p->vop + VP0_VACT);
	writel(hact, p->vop + VP0_POST_HACT);
	writel(vact, p->vop + VP0_POST_VACT);
	writel(0, p->vop + VP0_POST_SCALE_CTRL);
	writel(0, p->vop + ES0_DSP_ST);
	writel(size, p->vop + ES0_ACT);
	writel(size, p->vop + ES0_DSP);
	writel(framebuffer, p->vop + ES0_YRGB_ADDR);
	writel(m->width, p->vop + ES0_VIR); /* RGB stride, in words. */
	writel(0, p->vop + ES0_CTRL0);
	writel(0xa << 4 | 0xb << 12, p->vop + ES0_CTRL1); /* AXI read IDs */
	writel(0, p->vop + ES0_SCL_CTRL);
	writel(BIT(0), p->vop + ES0_REGION0_CTRL); /* Region enabled, XRGB8888 */
	vop_commit(p);
	writel(BIT(16) | VOP_VP0_FRAME_START, p->vop + VOP_VP0_INT_CLR);
	p->scanout = true;
	writel(15, p->vop + VP0_DSP_CTRL); /* AAAA packing for HDMI, exit standby. */
	vop_commit(p);
	return readl_poll_timeout(p->vop + VOP_VP0_INT_RAW, val,
				  val & VOP_VP0_FRAME_START, 50000);
}

static int rk3588_video_remove(struct udevice *dev)
{
	struct rk3588_boot_video *p = dev_get_priv(dev);
	struct video_uc_plat *uc = dev_get_uclass_plat(dev);
	const struct rk3588_hdmi_reset *rst = &hdmi_reset[p->port];
	void __iomem *cru = (void __iomem *)CRU_BASE;
	u32 val;

	if (p->scanout) {
		/* Retire the plane at a frame boundary, then stop the port. */
		writel(0, p->vop + ES0_REGION0_CTRL);
		vop_commit(p);
		readl_poll_timeout(p->vop + VOP_CFG_DONE, val, !(val & BIT(0)), 50000);
		writel(VOP_VP0_HOLD_VALID << 16 | VOP_VP0_HOLD_VALID,
		       p->vop + VOP_VP0_INT_CLR);
		writel(VOP_VP_STANDBY, p->vop + VP0_DSP_CTRL);
		/* Gating ACLK before DSP_HOLD_VALID can stall the memory bus. */
		if (readl_poll_timeout(p->vop + VOP_VP0_INT_RAW, val,
				       val & VOP_VP0_HOLD_VALID, 50000))
			puts("RKdev: display standby timed out; resetting VOP\n");
		writel(0, p->vop + VOP_IF_EN);
		reset_pulse(rst->bridge);
		reset_pulse(RST_VOP_DCLK0);
		reset_pulse(RST_VOP_ACLK);
		reset_pulse(RST_VOP_HCLK);
		p->scanout = false;
	}
	if (p->phy_started) {
		/* Reparent the port before removing its PHY pixel clock. */
		gate_enable(52, BIT(10));
		hiword(cru + RK3588_CLKSEL_CON(112), GENMASK(8, 7), 0);
		reset_pulse(rst->pclk);
		writel(0, p->phy + 0x207 * 4);
		reset_set(rst->lane, true);
		reset_set(rst->common, true);
		reset_set(rst->init, true);
		hiword(p->phy_grf, PHY_PLL_EN | PHY_BIAS_EN | PHY_BG_EN, 0);
		p->phy_started = false;
	}
	free(p->fb);
	p->fb = NULL;
	uc->base = 0;
	uc->size = 0;
	return 0;
}

static int rk3588_video_probe(struct udevice *dev)
{
	struct rk3588_boot_video *p = dev_get_priv(dev);
	struct video_uc_plat *uc = dev_get_uclass_plat(dev);
	struct video_priv *video = dev_get_uclass_priv(dev);
	const struct rk3588_hdmi_mode *mode;
	ofnode node = dev_ofnode(dev);
	bool hdmi = false;
	ulong ref, base, size;
	int ret;

	if (!dev_read_prop(dev, "u-boot,hdmi", NULL))
		return -ENODEV;
	p->vop = dev_read_addr_ptr(dev);
	p->pmu = phandle_regs(node, "rockchip,pmu");
	p->grf = phandle_regs(node, "rockchip,grf");
	p->vop_grf = phandle_regs(node, "rockchip,vop-grf");
	p->vo_grf = phandle_regs(node, "rockchip,vo1-grf");
	if (!p->vop || !p->pmu || !p->grf || !p->vop_grf || !p->vo_grf)
		return -EINVAL;

	ret = qp_find_sink(dev, p);
	if (ret)
		return ret;
	ret = rk3588_display_clocks(dev, p, &ref);
	if (ret) {
		printf("RKdev: HDMI%u display clocks unavailable (%d)\n", p->port, ret);
		return ret;
	}
	qp_init(p, ref);
	mode = qp_select_mode(p, &hdmi);
	size = (ulong)mode->width * mode->height * 4;
	p->fb = memalign(4096, size);
	if (!p->fb)
		return -ENOMEM;
	base = map_to_sysmem(p->fb);
	if (base > U32_MAX - (size - 1)) {
		ret = -ERANGE;
		goto err_fb;
	}
	memset(p->fb, 0, size);
	flush_dcache_range((ulong)p->fb, ALIGN((ulong)p->fb + size, ARCH_DMA_MINALIGN));

	ret = rk3588_hdptx_enable(p, mode);
	if (ret) {
		printf("RKdev: HDMI%u PHY did not start (%d)\n", p->port, ret);
		goto err_fb;
	}

	ret = vop_enable(p, mode, base);
	if (ret) {
		printf("RKdev: HDMI%u scanout did not start (%d)\n", p->port, ret);
		goto err_fb;
	}
	ret = qp_enable_video(p, mode, hdmi);
	if (ret) {
		printf("RKdev: HDMI%u display link clocks unavailable (%d)\n", p->port, ret);
		goto err_fb;
	}
	uc->base = base;
	uc->size = size;
	video->xsize = mode->width;
	video->ysize = mode->height;
	video->bpix = VIDEO_BPP32;
	video->format = VIDEO_X8R8G8B8;
	video_set_flush_dcache(dev, true);
	printf("RKdev: HDMI%u %ux%u @ 60 Hz\n", p->port, mode->width, mode->height);
	return 0;

err_fb:
	rk3588_video_remove(dev);
	return ret;
}

static int rk3588_video_bind(struct udevice *dev)
{
	struct video_uc_plat *uc = dev_get_uclass_plat(dev);

	/* The optional display allocates its buffer when probed, after relocation. */
	uc->hide_logo = true;
	return 0;
}

static const struct udevice_id rk3588_video_ids[] = {
	{ .compatible = "rockchip,rk3588-vop" },
	{ }
};

U_BOOT_DRIVER(rkdev_rk3588_video) = {
	.name = "rkdev_rk3588_video",
	.id = UCLASS_VIDEO,
	.of_match = rk3588_video_ids,
	.bind = rk3588_video_bind,
	.probe = rk3588_video_probe,
	.remove = rk3588_video_remove,
	.priv_auto = sizeof(struct rk3588_boot_video),
	.flags = DM_FLAG_OS_PREPARE,
};

static const struct udevice_id rk3588_qp_ids[] = {
	{ .compatible = "rockchip,rk3588-dw-hdmi-qp" },
	{ }
};

U_BOOT_DRIVER(rkdev_rk3588_qp) = {
	.name = "rkdev_rk3588_qp",
	.id = UCLASS_DISPLAY,
	.of_match = rk3588_qp_ids,
};
