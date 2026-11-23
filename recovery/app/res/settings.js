(() => {
    'use strict';
    const tag = (name, text, cls) => {
        const e = document.createElement(name);
        e.textContent = text ?? '';
        if (cls) e.className = cls;
        return e;
    };
    const get = id => document.getElementById(id);
    const button = (label, fn, cls = 'btn-boot-refresh') => {
        const b = tag('button', label, cls);
        b.type = 'button';
        b.addEventListener('click', fn);
        return b;
    };
    async function api(path, body) {
        const r = await fetch('/api/' + path, body === undefined ? {} : {
            method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(body)
        });
        const data = await r.json();
        if (!r.ok || data.error) throw new Error(data.error || '请求失败 (' + r.status + ')');
        return data;
    }
    function field(label, control) {
        const l = tag('label', '', 'setting-field');
        l.append(tag('span', label), control);
        return l;
    }
    function input(value, type, onInput) {
        const i = document.createElement('input');
        i.type = type;
        if (type === 'checkbox') i.checked = value;
        else i.value = value ?? '';
        if (onInput) i.addEventListener('input', () => onInput(type === 'checkbox' ? i.checked : i.value));
        return i;
    }
    function select(options, current, onChange) {
        const s = document.createElement('select');
        for (const [key, label] of options) s.add(new Option(label, key));
        s.value = current;
        if (onChange) s.addEventListener('change', () => onChange(s.value));
        return s;
    }
    function status() {
        const p = tag('p', '', 'settings-message');
        p.setAttribute('role', 'status');
        return p;
    }
    function message(p, text, error = false) {
        p.textContent = text;
        p.classList.toggle('settings-error', error);
    }
    const configs = ['WAN', 'LAN', 'Wi-Fi', '防火墙'];
    const trialClock = t => t && {...t, expires: performance.now() + 1000 * t.remaining_seconds};
    const secondsLeft = t => Math.max(0, Math.ceil((t.expires - performance.now()) / 1000));
    let netPage = 'WAN', netState, draft, netTrial, netTimer, fwTrial, fwTimer;
    let netForm, netBanner, netActual, netError;
    function openNetwork() {
        if (!netForm) {
            const mount = get('network-settings');
            const tabs = tag('div', '', 'settings-tabs');
            for (const name of configs) {
                tabs.append(button(name, () => switchNetwork(name)));
                tabs.lastChild.dataset.setting = name;
            }
            netActual = status();
            netBanner = tag('div', '', 'settings-trial');
            netBanner.hidden = true;
            netError = status();
            netForm = tag('div');
            mount.replaceChildren(tabs, netActual, netBanner, netError, netForm);
            switchNetwork(netPage);
        }
        refreshNetwork(true);
    }
    function switchNetwork(name) {
        netPage = name;
        for (const b of get('network-settings').querySelectorAll('[data-setting]')) {
            b.classList.toggle('selected', b.dataset.setting === name);
            b.setAttribute('aria-pressed', b.dataset.setting === name);
        }
        const fw = name === '防火墙';
        get('network-firewall').hidden = !fw;
        netForm.hidden = fw;
        if (fw) loadFirewallStatus();
        else renderNetworkForm();
    }
    async function refreshNetwork(reset) {
        try {
            const state = await api('network/config');
            const wasTrial = !!netTrial;
            netState = state;
            netTrial = trialClock(state.trial);
            if (reset || !draft || (wasTrial && !netTrial)) {
                draft = state.config ? structuredClone(state.config) : null;
                renderNetworkForm();
            }
            if (state.config) {
                const wan = state.addresses[state.config.wan.port] || '未连接';
                const lan = state.lan_actual || '未就绪';
                message(netActual, 'WAN：' + wan + '　 LAN：' + lan +
                    (state.config.wifi.mode === 'ap' ? '　 Wi-Fi：' + state.config.wifi.ssid : ''));
            } else message(netActual, '');
            message(netError, state.warning || state.message || '', !!state.warning);
            showNetworkTrial();
        } catch (e) {
            message(netError, '未能联系恢复系统：' + e.message, true);
            showNetworkTrial();
        }
    }
    function showNetworkTrial() {
        clearTimeout(netTimer);
        netBanner.hidden = !netTrial;
        const pending = !!netTrial;
        for (const list of netForm.querySelectorAll('fieldset')) list.disabled = pending || !netState?.ready;
        if (!pending) return;
        const t = netTrial, sec = secondsLeft(t);
        const link = tag('a', t.lan_url);
        link.href = t.lan_url;
        const ready = t.status === 'ready' && sec > 0 && !netState?.warning;
        const ok = button('确认配置', () => finishNetwork(t.id, 'confirm'), 'btn-boot-save');
        const undo = button('回退', () => finishNetwork(t.id, 'revert'));
        ok.disabled = !ready;
        undo.disabled = t.status !== 'ready' || sec === 0;
        netBanner.replaceChildren(
            tag('span', t.status === 'applying' ? '正在切换网络，必要时请重新连接。' :
                '试用：' + sec + ' 秒后自动回退。如地址改变，请访问 '), link, ok, undo);
        netTimer = setTimeout(() => refreshNetwork(false), 1000);
    }
    async function finishNetwork(id, action) {
        try {
            await api('network/config', {id, action});
            await refreshNetwork(true);
        } catch (e) { message(netError, e.message, true); }
    }
    async function submitNetwork(action) {
        try {
            const t = await api('network/config', action === 'apply' ? {action, config:draft} : {action});
            netTrial = trialClock(t);
            message(netError, '');
            showNetworkTrial();
        } catch (e) { message(netError, e.message, true); }
    }
    function renderNetworkForm() {
        if (!netState?.supported || !draft) {
            netForm.replaceChildren(tag('p', netState?.message || '正在读取板载网口…', 'settings-message'));
            return;
        }
        const c = draft;
        const panel = tag('fieldset', '', 'settings-fields');
        const refresh = () => renderNetworkForm();
        if (netPage === 'WAN') {
            const ports = netState.ports.filter(p => !p.wireless || netState.station);
            panel.append(field('WAN 网口', select(ports.map(p => [p.id, p.label + (netState.names[p.id] ?
                ' (' + netState.names[p.id] + ')' : ' (未检测到)')]), c.wan.port, v => { c.wan.port = v; })),
                field('连接方式', select([['dhcp','自动获取 DHCP'],['static','静态 IPv4'],['off','关闭']], c.wan.mode,
                    v => { c.wan.mode = v; refresh(); })));
            if (c.wan.mode === 'static') {
                panel.append(field('IPv4 / 前缀', input(c.wan.address, 'text', v => c.wan.address = v.trim())),
                    field('网关', input(c.wan.gateway, 'text', v => c.wan.gateway = v.trim())));
            }
            panel.append(field('DNS（空格分隔）', input((c.wan.dns || []).join(' '), 'text',
                v => c.wan.dns = v.trim().split(/[ ,]+/).filter(Boolean))));
        }
        if (netPage === 'LAN') {
            panel.append(field('管理 IPv4 / 前缀', input(c.lan.address, 'text', v => c.lan.address = v.trim())));
            for (const p of netState.ports.filter(p => !p.wireless)) {
                panel.append(field('LAN 网口：' + p.label, input((c.lan.ports || []).includes(p.id), 'checkbox', checked => {
                    c.lan.ports = (c.lan.ports || []).filter(id => id !== p.id);
                    if (checked) c.lan.ports.push(p.id);
                })));
            }
            panel.append(field('DHCP 服务', input(c.lan.dhcp, 'checkbox', v => { c.lan.dhcp = v; refresh(); })));
            if (c.lan.dhcp) panel.append(field('地址池起点', input(c.lan.start, 'text', v => c.lan.start = v.trim())),
                field('地址池终点', input(c.lan.end, 'text', v => c.lan.end = v.trim())));
            panel.append(field('通过 WAN 共享网络 (NAT)', input(c.lan.nat, 'checkbox', v => c.lan.nat = v)));
        }
        if (netPage === 'Wi-Fi') {
            const modes = [['off','关闭'], ['ap','恢复热点']];
            if (netState.station) modes.push(['station','无线客户端']);
            panel.append(field('工作模式', select(modes, c.wifi.mode, v => { c.wifi.mode = v; refresh(); })));
            if (c.wifi.mode !== 'off') {
                panel.append(field('网络名称 (SSID)', input(c.wifi.ssid, 'text', v => c.wifi.ssid = v)));
                if (c.wifi.mode === 'ap') {
                    panel.append(field('频段', select(netState.ap_bands.map(b => [b, b + ' GHz']), c.wifi.band, v => {
                        c.wifi.band = v; c.wifi.channel = v === '5' ? 36 : 6; refresh();
                    })), field('信道', input(c.wifi.channel, 'number', v => c.wifi.channel = Number(v))));
                } else panel.append(tag('p', '无线客户端模式需在 WAN 页将网口选为 Wi-Fi。', 'settings-message'));
                const replace = c.wifi.password !== undefined;
                panel.append(field(netState.secured ? '更换现有密码' : '设置密码', input(replace, 'checkbox', v => {
                    if (v) c.wifi.password = ''; else delete c.wifi.password;
                    refresh();
                })));
                if (replace) {
                    const pw = input(c.wifi.password, 'password', v => c.wifi.password = v);
                    pw.autocomplete = 'new-password';
                    pw.placeholder = '留空即开放网络';
                    panel.append(field('新密码', pw));
                }
            }
        }
        const buttons = tag('div', '', 'settings-actions');
        buttons.append(button('试用配置', () => submitNetwork('apply'), 'btn-boot-save'),
            button('试用板包默认配置', () => submitNetwork('defaults')), button('重新读取', () => refreshNetwork(true)));
        panel.disabled = !!netTrial || !netState.ready;
        netForm.replaceChildren(panel, buttons);
        for (const b of buttons.children) {
            if (b.textContent !== '重新读取') b.disabled = panel.disabled;
        }
        if (!netState.persistent) netForm.append(tag('p', '配置仅在本次启动中生效。', 'settings-message'));
    }

    function drawFirewallTrial(data) {
        clearTimeout(fwTimer);
        fwTrial = trialClock(data.trial);
        const box = get('fw-trial');
        box.hidden = !fwTrial && !data.message;
        box.replaceChildren();
        if (!fwTrial) {
            box.textContent = data.message || '';
            return;
        }
        const t = fwTrial;
        const remain = secondsLeft(t);
        const send = async operation => {
            try {
                await api('firewall/trial', {trial:t.id, operation});
                await loadFirewallStatus();
            } catch (e) { alert(e.message); }
        };
        box.append(tag('span', t.status === 'applying' ? '正在应用规则…' : '试用中，' + remain + ' 秒后自动回退。'));
        if (t.status === 'ready' && remain > 0) {
            box.append(button('确认规则', () => send('confirm'), 'btn-boot-save'), button('回退', () => send('revert')));
        }
        fwTimer = setTimeout(() => {
            if (netPage === '防火墙') loadFirewallStatus();
        }, 1000);
    }

    async function openSSH() {
        const mount = get('ssh-settings');
        try {
            const d = await api('ssh/config');
            if (!d.supported) {
                mount.replaceChildren(tag('p', '此板包未启用可配置 SSH。', 'settings-message'));
                return;
            }
            const form = tag('fieldset', '', 'settings-fields');
            const msg = status(), c = d.config;
            const keys = document.createElement('textarea');
            keys.rows = 4;
            keys.value = c.keys;
            keys.addEventListener('input', () => c.keys = keys.value.replace(/\r\n/g, '\n'));
            form.append(field('启用 SSH', input(c.enabled,'checkbox',v => c.enabled = v)),
                field('监听端口', input(c.port,'number',v => c.port = Number(v))),
                field('允许密码登录', input(c.password,'checkbox',v => c.password = v)),
                field('OpenSSH 公钥（每行一条）', keys));
            const save = button('保存并应用', async () => {
                save.disabled = true;
                try {
                    const saved = await api('ssh/config', c);
                    message(msg, saved.message);
                } catch (e) { message(msg, e.message, true); }
                finally { save.disabled = false; }
            }, 'btn-boot-save');
            message(msg, d.message || (d.persistent ? '' : '配置仅在本次启动中生效。'));
            mount.replaceChildren(form, save, msg);
        } catch (e) {
            mount.replaceChildren(button('重试', openSSH), tag('p', e.message, 'settings-error'));
        }
    }

    class DeviceFiles {
        constructor(details) {
            this.details = details;
            this.kind = details.dataset.media;
            const upgrade = this.kind === 'upgrade';
            this.body = details.querySelector('.device-media-body');
            this.msg = status();
            this.progress = status();
            this.constraint = status();
            this.vol = select([['','选择分区']], '', () => this.selectVolume());
            this.list = tag('div', '', 'device-file-list');
            this.path = tag('span', '/');
            this.targets = select([['','请先选择来源分区']], '');
            this.image = tag('strong', '未选镜像');
            this.meta = tag('strong', '未选清单');
            this.submit = button(upgrade ? '从设备升级' : '从设备刷写', () => this.start(), 'btn-boot-save');
            this.submit.disabled = true;
            this.refresh = button('刷新存储', () => this.load());
            const source = tag('div', '', 'settings-actions');
            source.append(this.vol, this.refresh);
            const path = tag('div', '', 'settings-actions');
            path.append(button('上一级', () => this.parent()), this.path);
            const selections = tag('div', '', 'device-choices');
            selections.append(tag('span', '镜像：'), this.image);
            if (upgrade) selections.append(tag('span','配套清单：'), this.meta);
            this.body.append(source, path, this.list, selections);
            if (!upgrade) {
                this.body.append(field('写入目标', this.targets),
                    tag('p','来源所在的整块盘及其所有分区不能作为写入目标。', 'settings-message'));
                this.targets.addEventListener('change', () => this.validate());
            } else {
                this.body.append(tag('p','请从另一块盘选择同一版本的 IMG 和 JSON。', 'settings-message'));
            }
            this.body.append(this.submit, this.msg, this.constraint, this.progress);
            this.dir = '.';
            this.seq = 0;
            details.addEventListener('toggle', () => { if (details.open && !this.loaded) this.load(); });
        }

        async load() {
            const seq = ++this.seq;
            this.volume = null;
            message(this.constraint, '');
            this.setSelected();
            this.vol.replaceChildren(new Option('选择来源分区', ''));
            this.list.replaceChildren();
            this.targets.replaceChildren(new Option('请先选择来源分区', ''));
            this.path.textContent = '/';
            this.validate();
            try {
                const volumes = await api('media');
                if (seq !== this.seq) return;
                this.loaded = true;
                this.volumes = volumes;
                for (const v of volumes) this.vol.add(new Option('/dev/' + v.name + ' · ' + v.size, v.id));
                message(this.msg, volumes.length === 0 ? '未发现可读取的设备，请插入存储后刷新。' : '');
            } catch (e) { if (seq === this.seq) message(this.msg, e.message, true); }
        }

        async selectVolume() {
            this.volume = this.volumes.find(v => v.id === this.vol.value);
            this.setSelected();
            this.targets.replaceChildren(new Option('选择写入目标', ''));
            this.targetList = [];
            this.upgradeAllowed = false;
            message(this.constraint, '');
            this.validate();
            if (!this.volume) {
                ++this.seq;
                this.dir = '.';
                this.path.textContent = '/';
                this.list.replaceChildren();
                return;
            }
            const ref = this.volume;
            this.browse('.');
            try {
                const q = new URLSearchParams({id:ref.id, name:ref.name});
                if (this.kind === 'upgrade') {
                    q.set('kind', 'upgrade');
                    await api('media/targets?' + q);
                    if (this.volume === ref) this.upgradeAllowed = true;
                    this.validate();
                    return;
                }
                const targets = await api('media/targets?' + q);
                if (this.volume !== ref) return;
                this.targetList = targets;
                for (const x of targets) this.targets.add(new Option((x.type || '') + ' ' + x.path + ' · ' + x.size, x.ref.id));
                if (!targets.length) this.targets.firstChild.textContent = '没有可用的异盘目标';
            } catch (e) { if (this.volume === ref) message(this.constraint, e.message, true); }
            this.validate();
        }

        async browse(dir) {
            if (!this.volume) return;
            this.dir = dir;
            this.path.textContent = dir === '.' ? '/' : '/' + dir;
            const seq = ++this.seq;
            const q = new URLSearchParams({id:this.volume.id, name:this.volume.name, dir});
            this.list.replaceChildren(tag('span', '正在读取…'));
            try {
                const files = await api('media/files?' + q);
                if (seq !== this.seq) return;
                this.list.replaceChildren();
                message(this.msg, '');
                const upgrade = this.kind === 'upgrade';
                for (const f of files) {
                    const img = /\.img$/i.test(f.name), meta = /\.json$/i.test(f.name);
                    if (!f.dir && (upgrade ? !(img || meta) : meta)) continue;
                    const b = button((f.dir ? '📁 ' : '↳ ') + f.name, () => {
                        const path = dir === '.' ? f.name : dir + '/' + f.name;
                        if (f.dir) this.browse(path);
                        else if (meta) this.setSelected(this.imagePath, path);
                        else {
                            const expected = f.name.replace(/\.img$/i,'.json');
                            const companion = upgrade && files.find(y => !y.dir && y.name === expected);
                            this.setSelected(path, companion ? (dir === '.' ? '' : dir+'/')+companion.name : this.metaPath);
                        }
                    }, 'device-file');
                    this.list.append(b);
                }
                if (!this.list.children.length) this.list.append(tag('span','此目录没有可选镜像或子目录。'));
            } catch (e) {
                if (seq !== this.seq) return;
                this.list.replaceChildren();
                message(this.msg, e.message, true);
            }
        }

        parent() {
            if (this.dir !== '.') this.browse(this.dir.split('/').slice(0,-1).join('/') || '.');
        }

        setSelected(image = '', meta = '') {
            this.imagePath = image;
            this.metaPath = meta;
            this.image.textContent = image || '未选镜像';
            this.meta.textContent = meta || '未选清单';
            this.validate();
        }

        validate() {
            this.submit.disabled = !!mediaActive || !this.imagePath || !this.volume ||
                (this.kind === 'upgrade' ? !this.metaPath || !this.upgradeAllowed : !this.targets.value);
        }

        async start() {
            if (this.submit.disabled) return;
            const upgrade = this.kind === 'upgrade';
            const target = upgrade ? null : this.targetList.find(x => x.ref.id === this.targets.value);
            if (!upgrade && !target) return;
            if (!confirm(upgrade
                ? '从设备存储更新引导和恢复系统？请保持设备供电。'
                : '确定把 ' + this.imagePath + ' 写到 ' + target.path + '？目标上的数据会被覆盖。')) return;
            this.submit.disabled = true;
            const job = {volume:{name:this.volume.name, id:this.volume.id}, image:this.imagePath,
                manifest:this.metaPath};
            if (target) job.target = target.ref;
            try {
                await api('media/' + this.kind, job);
                await pollMedia();
            } catch (e) {
                message(this.msg, e.message, true);
                this.validate();
            }
        }
    }

    let mediaActive = false, mediaTimer;
    const media = Array.from(document.querySelectorAll('details[data-media]'), el => new DeviceFiles(el));
    async function pollMedia() {
        clearTimeout(mediaTimer);
        try {
            const j = await api('media/job');
            mediaActive = !!j.id && !['done','error'].includes(j.stage);
            for (const ui of media) {
                ui.validate();
                if (!j.id || ui.kind !== j.kind) continue;
                if (mediaActive) ui.details.open = true;
                const phase = {preparing:'正在打开源卷与镜像', checking:'正在校验升级镜像',
                    writing:'正在直接写入并读回校验', done:'完成', error:'未完成'}[j.stage] || j.stage;
                let progress = '';
                if (j.task_id && mediaActive) {
                    const info = await api('upload_status');
                    if (info.task_id === j.task_id && info.written > 0) {
                        progress = ' · 已写入 ' + fmtBytes(info.written) + (info.expanded_total > 0 ?
                            ' / ' + fmtBytes(info.expanded_total) : '');
                    }
                }
                message(ui.progress, j.image + '：' + phase + progress + (j.message ? ' — ' + j.message : ''), j.stage === 'error');
            }
        } catch (e) {
            if (mediaActive) for (const ui of media) message(ui.progress, '暂时无法读取进度，任务仍由设备执行。', true);
        }
        mediaTimer = setTimeout(pollMedia, mediaActive ? 1000 : 4500);
    }
    pollMedia();
    window.rkSettings = {openNetwork, openSSH, drawFirewallTrial, firewallTrialActive:() => !!fwTrial};
})();
