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
        if (recoveryLoginRequired(r)) reopenLogin();
        const data = await r.json();
        if (!r.ok || data?.error) throw new Error(data?.error || '请求失败 (' + r.status + ')');
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

    let managementDraft, managementLoading = false;
    async function openManagement() {
        if (managementLoading || managementDraft?.hasEdits()) return;
        const mount = get('management-settings');
        managementLoading = true;
        try {
            renderManagement(await api('management'));
        } catch (e) {
            mount.replaceChildren(button('重试', openManagement), tag('p', e.message, 'settings-error'));
        } finally { managementLoading = false; }
    }

    function renderManagement(state) {
        const mount = get('management-settings');
        const form = tag('form', '', 'management-form');
        const groups = [], msg = status();
        const section = title => {
            const controls = tag('fieldset', '', 'settings-fields management-group');
            controls.append(tag('legend', title));
            form.append(controls);
            groups.push(controls);
            return controls;
        };
        const secret = kind => {
            const p = input('', 'password');
            p.autocomplete = kind;
            p.maxLength = 128;
            return p;
        };
        const username = input(state.username, 'text');
        const password = secret('new-password'), confirm = secret('new-password');
        username.autocomplete = 'off';
        username.maxLength = 32;
        username.spellcheck = false;
        password.placeholder = '留空保留现有密码';
        if (state.username) {
            const account = section('恢复系统账号');
            account.append(field('账号', username));
            account.append(field('新密码', password), field('确认新密码', confirm));
        }
        const c = state.ssh ? {...state.ssh} : null;
        const ssh = section('SSH');
        if (c) {
            const keys = document.createElement('textarea');
            keys.rows = 3;
            keys.value = c.keys;
            keys.maxLength = 4096;
            keys.spellcheck = false;
            keys.addEventListener('input', () => c.keys = keys.value.replace(/\r\n/g, '\n'));
            const port = input(c.port, 'number', v => c.port = Number(v));
            port.min = 1;
            port.max = 65535;
            const publicKeys = field('OpenSSH 公钥（每行一条）', keys);
            publicKeys.classList.add('management-wide');
            ssh.append(field('启用 SSH', input(c.enabled, 'checkbox', v => c.enabled = v)),
                field('监听端口', port), field('允许密码登录', input(c.password, 'checkbox', v => c.password = v)), publicKeys);
        } else ssh.append(tag('p', '未安装', 'settings-message'));
        message(msg, state.message || (state.persistent ? '' : '设置仅在本次启动中生效。'));

        if (!state.username) {
            managementDraft = null;
            for (const group of groups) group.disabled = true;
            mount.replaceChildren(form, msg);
            return;
        }

        const accountChanged = () => username.value.trim() !== state.username || !!password.value;
        const sshChanged = () => c && (c.enabled !== state.ssh.enabled || c.port !== state.ssh.port ||
            c.password !== state.ssh.password || c.keys !== state.ssh.keys);
        let saving = false;
        const hasEdits = () => saving || !!(accountChanged() || sshChanged() || confirm.value);
        managementDraft = {hasEdits};
        const actions = tag('div', '', 'settings-actions');
        const save = tag('button', '保存并应用', 'btn-boot-save');
        save.type = 'submit';
        const discard = button('放弃更改', () => renderManagement(state));
        const controls = () => {
            save.disabled = saving || (!accountChanged() && !sshChanged());
            discard.disabled = saving || !hasEdits();
            for (const group of groups) group.disabled = saving;
        };
        actions.append(save, discard);
        form.append(actions, msg);
        form.addEventListener('input', controls);
        form.addEventListener('submit', async event => {
            event.preventDefault();
            if (save.disabled) return;
            if (password.value !== confirm.value) {
                message(msg, '两次输入的密码不一致', true);
                confirm.focus();
                return;
            }
            const edit = {};
            if (accountChanged()) edit.account = {username: username.value.trim(), password: password.value};
            if (sshChanged()) edit.ssh = c;
            saving = true;
            controls();
            try {
                const state = await api('management', edit);
                if (state.login_required) reopenLogin(true);
                else renderManagement(state);
            }
            catch (e) {
                message(msg, e.message, true);
            } finally {
                saving = false;
                controls();
            }
        });
        mount.replaceChildren(form);
        controls();
    }

    const mediaIcons = {
        refresh: 'M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8M21 3v5h-5M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16M8 16H3v5',
        parent: 'M12 19V5m-7 7 7-7 7 7',
        folder: 'M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.7-1l-.8-1a2 2 0 0 0-1.7-1H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z',
        file: 'M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7ZM14 2v6h6',
        chevron: 'm9 18 6-6-6-6',
        check: 'm20 6-11 11-5-5'
    };
    function mediaIcon(name) {
        const ns = 'http://www.w3.org/2000/svg';
        const svg = document.createElementNS(ns, 'svg');
        const path = document.createElementNS(ns, 'path');
        for (const [key, value] of Object.entries({viewBox:'0 0 24 24', fill:'none',
            stroke:'currentColor', 'stroke-width':'1.7', 'stroke-linecap':'round',
            'stroke-linejoin':'round', 'aria-hidden':'true'})) svg.setAttribute(key, value);
        path.setAttribute('d', mediaIcons[name]);
        svg.append(path);
        return svg;
    }
    function mediaTool(icon, label, action) {
        const control = button('', action, 'device-media-tool');
        control.title = label;
        control.setAttribute('aria-label', label);
        control.append(mediaIcon(icon));
        return control;
    }

    class DeviceFiles {
        constructor(root, onChange = () => {}) {
            this.root = root;
            this.onChange = onChange;
            this.upgrade = root.dataset.media === 'upgrade';
            const body = root.querySelector('.device-media-body');
            this.msg = status();
            this.vol = select([], '', () => this.selectVolume());
            this.vol.setAttribute('aria-label', '来源分区');
            this.list = tag('div', '', 'device-file-list');
            this.path = tag('span', '/', 'device-directory-path');
            this.refresh = mediaTool('refresh', '刷新存储', () => this.load());
            this.up = mediaTool('parent', '上一级', () => this.parent());
            const source = tag('div', '', 'device-source');
            source.append(field('来源分区', this.vol), this.refresh);
            const path = tag('div', '', 'device-directory');
            path.append(this.up, this.path);
            const files = tag('div', '', 'device-browser');
            files.append(path, this.list);
            body.append(source, files, this.msg);
            if (this.upgrade) {
                this.progress = status();
                this.constraint = status();
                this.image = tag('dd', '未选择文件');
                this.meta = tag('dd', '未选择文件');
                const selections = tag('dl', '', 'device-choices');
                selections.append(tag('dt', '镜像'), this.image, tag('dt', '校验清单'), this.meta);
                const actions = tag('div', '', 'device-actions');
                this.submit = button('升级恢复系统', () => this.startUpgrade(), 'btn-boot-save');
                actions.append(this.submit);
                body.append(selections, actions, this.constraint, this.progress);
                root.addEventListener('toggle', () => { if (root.open) this.ensureLoaded(); });
            }
            this.volumes = [];
            this.dir = '.';
            this.seq = 0;
            this.selection = 0;
            this.emptyList('请选择来源分区');
            this.validate();
        }

        ensureLoaded() {
            if (!this.loaded && !this.loadingVolumes) this.load();
        }

        emptyList(text) {
            this.list.replaceChildren(tag('p', text, 'device-file-empty'));
        }

        showDirectory(dir) {
            this.dir = dir;
            this.path.textContent = dir === '.' ? '/' : '/' + dir;
            this.path.title = this.path.textContent;
        }

        async load() {
            const seq = ++this.seq;
            ++this.selection;
            this.loadingVolumes = true;
            this.volume = null;
            this.volumes = [];
            this.upgradeAllowed = false;
            if (this.upgrade) message(this.constraint, '');
            message(this.msg, '');
            this.setSelected();
            this.vol.replaceChildren(new Option('正在读取…', ''));
            this.emptyList('正在读取存储设备…');
            this.showDirectory('.');
            this.validate();
            try {
                const volumes = await api('media');
                if (seq !== this.seq) return;
                this.loaded = true;
                this.volumes = volumes;
                this.vol.replaceChildren(new Option(volumes.length ? '选择分区' : '没有可读的分区', ''));
                for (const v of volumes) this.vol.add(new Option('/dev/' + v.name + ' · ' + v.size, v.id));
                this.emptyList(volumes.length ? '请选择来源分区' : '未发现可读的分区');
            } catch (e) {
                if (seq !== this.seq) return;
                this.vol.replaceChildren(new Option('读取失败', ''));
                this.emptyList('读取失败');
                message(this.msg, e.message, true);
            } finally {
                if (seq === this.seq) {
                    this.loadingVolumes = false;
                    this.validate();
                }
            }
        }

        async selectVolume() {
            const selection = ++this.selection;
            this.volume = this.volumes.find(v => v.id === this.vol.value);
            this.upgradeAllowed = false;
            if (this.upgrade) message(this.constraint, '');
            this.setSelected();
            if (!this.volume) {
                ++this.seq;
                this.showDirectory('.');
                this.emptyList('请选择来源分区');
                this.validate();
                return;
            }
            const ref = this.volume;
            this.browse('.');
            if (!this.upgrade) return;
            try {
                const q = new URLSearchParams({id:ref.id, name:ref.name, kind:'upgrade'});
                await api('media/targets?' + q);
                if (this.selection !== selection) return;
                this.upgradeAllowed = true;
            } catch (e) {
                if (this.selection !== selection) return;
                message(this.constraint, e.message, true);
            }
            this.validate();
        }

        async browse(dir) {
            if (!this.volume) return;
            this.showDirectory(dir);
            const seq = ++this.seq;
            const q = new URLSearchParams({id:this.volume.id, name:this.volume.name, dir});
            this.emptyList('正在读取目录…');
            this.validate();
            try {
                const files = await api('media/files?' + q);
                if (seq !== this.seq) return;
                this.list.replaceChildren();
                message(this.msg, '');
                const upgrade = this.upgrade;
                for (const f of files) {
                    const img = /\.img$/i.test(f.name), meta = /\.json$/i.test(f.name);
                    if (!f.dir && (upgrade ? !(img || meta) : meta)) continue;
                    const path = dir === '.' ? f.name : dir + '/' + f.name;
                    const b = button('', () => {
                        if (f.dir) this.browse(path);
                        else if (meta) this.setSelected(this.imagePath, path);
                        else {
                            const expected = f.name.replace(/\.img$/i,'.json');
                            const companion = upgrade && files.find(y => !y.dir && y.name === expected);
                            this.setSelected(path, companion ? (dir === '.' ? '' : dir+'/')+companion.name : this.metaPath);
                        }
                    }, 'device-file');
                    b.append(mediaIcon(f.dir ? 'folder' : 'file'), tag('span', f.name),
                        mediaIcon(f.dir ? 'chevron' : 'check'));
                    if (!f.dir) b.dataset.file = path;
                    this.list.append(b);
                }
                if (!this.list.children.length) this.emptyList('此目录没有可选文件');
                this.highlight();
                this.validate();
            } catch (e) {
                if (seq !== this.seq) return;
                this.emptyList('读取目录失败');
                message(this.msg, e.message, true);
            }
        }

        parent() {
            if (this.dir !== '.') this.browse(this.dir.split('/').slice(0,-1).join('/') || '.');
        }

        setSelected(image = '', meta = '') {
            this.imagePath = image;
            this.metaPath = meta;
            if (this.upgrade) {
                this.image.textContent = image ? '/' + image : '未选择文件';
                this.meta.textContent = meta ? '/' + meta : '未选择文件';
            }
            this.highlight();
            this.validate();
        }

        highlight() {
            for (const row of this.list.querySelectorAll('[data-file]')) {
                row.setAttribute('aria-pressed', String(row.dataset.file === this.imagePath || row.dataset.file === this.metaPath));
            }
        }

        selectedImage() {
            if (!this.volume || !this.imagePath) return null;
            const {name, id} = this.volume;
            return {volume:{name, id}, image:this.imagePath};
        }

        validate() {
            const busy = !!mediaActive || !!this.submitting;
            this.refresh.disabled = busy || !!this.loadingVolumes;
            this.vol.disabled = busy || !this.volumes.length;
            this.up.disabled = busy || this.dir === '.';
            for (const row of this.list.querySelectorAll('button')) row.disabled = busy;
            if (this.upgrade) this.submit.disabled = busy || !this.selectedImage() || !this.metaPath || !this.upgradeAllowed;
            this.onChange(busy ? null : this.selectedImage());
        }

        async startUpgrade() {
            if (this.submit.disabled) return;
            if (!confirm('从设备存储更新引导和恢复系统？请保持设备供电。')) return;
            this.submitting = true;
            this.validate();
            message(this.msg, '');
            try {
                acceptMediaJob(await api('media/upgrade', {...this.selectedImage(), manifest:this.metaPath}));
            } catch (e) {
                message(this.msg, e.message, true);
            } finally {
                this.submitting = false;
                this.validate();
            }
        }
    }

    class FlashForm {
        constructor() {
            this.form = get('flash-form');
            this.sources = get('flash-sources');
            this.input = get('firmware-file');
            this.choose = get('flash-choose-file');
            this.target = get('device-select');
            this.refresh = get('flash-refresh-targets');
            this.submit = get('btn-flash');
            this.dialog = get('flash-device-dialog');
            this.use = get('flash-use-file');
            this.picker = new DeviceFiles(this.dialog, selection => { this.use.disabled = !selection; });
            this.targets = new Map();
            this.targetRequest = 0;
            this.refresh.append(mediaIcon('refresh'));
            this.sources.addEventListener('change', () => {
                this.input.value = '';
                this.picker.setSelected();
                this.setFile(null);
            });
            this.choose.addEventListener('click', () => {
                if (this.form.elements['flash-source'].value === 'upload') this.input.click();
                else {
                    this.dialog.showModal();
                    this.picker.ensureLoaded();
                }
            });
            this.input.addEventListener('change', () => {
                const file = this.input.files[0];
                if (file && !fwNameOk(file.name)) {
                    this.input.value = '';
                    this.setFile(null);
                    message(get('flash-file-status'), '不支持的文件类型：' + file.name, true);
                } else {
                    this.setFile(file ? {file, label:file.name + ' · ' + fmtBytes(file.size)} : null);
                }
            });
            get('flash-cancel-file').addEventListener('click', () => this.dialog.close());
            this.use.addEventListener('click', () => {
                const selection = this.picker.selectedImage();
                if (!selection) return;
                this.dialog.close();
                this.setFile({...selection, label:'/dev/' + selection.volume.name + ':/' + selection.image});
            });
            // A cancelled dialog must not replace the file shown on the form.
            this.dialog.addEventListener('close', () => {
                const sameVolume = this.file?.volume?.id === this.picker.volume?.id;
                this.picker.setSelected(sameVolume ? this.file?.image : '');
            });
            this.refresh.addEventListener('click', () => this.loadTargets(this.target.value));
            this.target.addEventListener('change', () => this.validate());
            this.form.addEventListener('submit', e => {
                e.preventDefault();
                this.start();
            });
            this.validate();
        }

        setFile(file) {
            const sameSource = file && this.file && file.volume?.id === this.file.volume?.id;
            const keep = sameSource ? this.target.value : '';
            this.file = file;
            get('flash-file-name').textContent = file?.label || '未选择镜像';
            message(get('flash-file-status'), '');
            this.clearFeedback();
            this.loadTargets(keep);
        }

        async loadTargets(keep = '') {
            const request = ++this.targetRequest;
            const source = this.file?.volume;
            const status = get('flash-target-status');
            this.targets.clear();
            this.target.replaceChildren(new Option(this.file ? '正在读取…' : '请先选择镜像', ''));
            message(status, '');
            this.loadingTargets = !!this.file;
            this.validate();
            if (!this.file) return;
            try {
                const devices = await api(source ? 'media/targets?' + new URLSearchParams(source) : 'devices');
                if (request !== this.targetRequest) return;
                this.target.replaceChildren(new Option(devices?.length ? '请选择写入目标' : '没有可用的写入目标', ''));
                for (const d of devices || []) {
                    const key = d.ref?.id || d.path;
                    this.targets.set(key, d);
                    this.target.add(new Option([d.type, d.path, d.size, d.model].filter(Boolean).join(' · '), key));
                }
                if (this.targets.has(keep)) this.target.value = keep;
                if (!this.targets.size && source) message(status, '镜像所在的整块盘已排除。');
            } catch (e) {
                if (request !== this.targetRequest) return;
                this.target.replaceChildren(new Option('读取失败', ''));
                message(status, e.message, true);
            } finally {
                if (request === this.targetRequest) {
                    this.loadingTargets = false;
                    this.validate();
                }
            }
        }

        validate() {
            const running = this.sending || this.uploadTask || this.mediaJob;
            const busy = running || mediaActive;
            this.sources.disabled = this.choose.disabled = !!busy;
            this.target.disabled = busy || !this.targets.size;
            this.refresh.disabled = busy || !this.file || this.loadingTargets;
            this.submit.disabled = busy || !this.file || !this.targets.has(this.target.value);
            this.submit.textContent = running ? '执行中…' : '开始刷写';
        }

        async start() {
            if (this.submit.disabled) return;
            const selection = this.file;
            const target = this.targets.get(this.target.value);
            if (!confirm('镜像：' + selection.label + '\n写入目标：' + target.path +
                '\n\n目标上的数据会被覆盖。确定开始刷写？')) return;
            this.clearFeedback();
            this.sending = true;
            this.validate();
            appendLog('开始刷写: ' + selection.label + ' → ' + target.path, 'warn');
            try {
                if (selection.file) await this.upload(selection.file, target.path);
                else {
                    showResult('正在打开设备镜像…', 'info');
                    acceptMediaJob(await api('media/flash', {volume:selection.volume, image:selection.image, target:target.ref}));
                }
            } catch (e) {
                this.finish(e.message);
            } finally {
                this.sending = false;
                this.validate();
            }
        }

        async upload(file, target) {
            const body = new FormData();
            body.append('target_device', target);
            body.append('firmware_size', String(file.size));
            body.append('firmware', file);
            showResult('正在接收固件…', 'info');
            startUploadPoll();
            try {
                const resp = await fetch('/upload', {method:'POST', body});
                const data = await resp.json().catch(() => {
                    throw new Error('未收到刷写响应（HTTP ' + resp.status + '）');
                });
                if (!resp.ok || !data.task_id) throw new Error(data.error || '未取得刷写任务');
                this.uploadTask = data.task_id;
                showResult(data.message || '正在写入镜像…', 'info');
                this.watchTask(data.task_id, t => {
                    this.uploadTask = null;
                    this.finish(t.status === 'error' ? t.error || '未知错误' : '');
                    this.validate();
                });
            } finally {
                stopUploadPoll();
            }
        }

        clearFeedback() {
            this.stream?.close();
            this.task = '';
            hideProgress();
            get('result-area').style.display = 'none';
        }

        watchTask(id, onFinish) {
            if (this.task === id) return;
            this.stream?.close();
            this.task = id;
            const stream = this.stream = new EventSource('/api/progress?id=' + encodeURIComponent(id));
            stream.onmessage = e => {
                let t;
                try { t = JSON.parse(e.data); } catch { return; }
                updateProgressUI(t);
                showProgress();
                if (t.status === 'done' || t.status === 'error') {
                    stream.close();
                    onFinish?.(t);
                }
            };
            // EventSource reconnects on temporary connection failures.
            stream.onerror = () => {
                if (stream.readyState === EventSource.CLOSED)
                    onFinish?.({status:'error', error:'进度连接中断，请确认设备上的任务状态。'});
            };
        }

        finish(error, success = '刷写完成！') {
            const result = error ? '刷写失败：' + error : success;
            showResult(result, error ? 'error' : 'success');
            appendLog(result, error ? 'error' : 'success');
        }

        observe(j) {
            if (this.mediaJob && this.mediaJob !== j.id) {
                this.mediaJob = null;
                this.finish('无法确认设备上的任务结果。');
            }
            if (j?.kind === 'flash' && !this.uploadTask && !(this.sending && this.file?.file)) {
                if (mediaActive && this.mediaJob !== j.id) {
                    this.clearFeedback();
                    this.mediaJob = j.id;
                    this.mediaStage = '';
                    if (this.dialog.open) this.dialog.close();
                }
                if (this.mediaJob && this.mediaJob === j.id) {
                    if (j.task_id) this.watchTask(j.task_id);
                    if (!mediaActive) {
                        this.mediaJob = null;
                        this.finish(j.stage === 'error' ? j.message || '未知错误' : '', j.message || '刷写完成！');
                    } else if (this.mediaStage !== j.stage) {
                        showResult(j.stage === 'writing' ? '正在写入和校验镜像…' : '正在打开设备镜像…', 'info');
                        this.mediaStage = j.stage;
                    }
                }
            }
            this.picker.validate();
            this.validate();
        }
    }

    let mediaActive = false, mediaTimer, mediaRevision = 0;
    const flash = new FlashForm();
    const upgrade = new DeviceFiles(document.querySelector('[data-media="upgrade"]'));

    class RecoverySourcePicker {
        constructor(media) {
            this.media = media;
            this.upload = get('recovery-upload-source');
            this.image = get('recovery-image');
            this.manifest = get('recovery-manifest');
            this.result = get('recovery-result');
            this.radios = [...document.querySelectorAll('input[name="recovery-source"]')];
            this.source = '';
            for (const radio of this.radios) radio.addEventListener('change', () => this.set(radio.value, true));
            this.set(this.radios.find(radio => radio.checked)?.value || 'upload', false);
        }

        set(source, clearSelection) {
            const next = source === 'device' ? 'device' : 'upload';
            const changed = this.source !== next;
            this.source = next;
            for (const radio of this.radios) radio.checked = radio.value === next;
            const usingDevice = next === 'device';
            this.upload.hidden = usingDevice;
            this.media.root.hidden = !usingDevice;
            this.media.root.open = usingDevice;
            if (usingDevice) this.media.ensureLoaded();
            if (changed && clearSelection) {
                this.image.value = '';
                this.manifest.value = '';
                this.media.setSelected();
                this.result.className = 'update-status';
                this.result.textContent = '';
            }
            this.validate();
        }

        selectDevice() {
            this.set('device', false);
        }

        validate() {
            for (const radio of this.radios) radio.disabled = mediaActive;
        }
    }

    const updateSources = new RecoverySourcePicker(upgrade);
    function showMediaJob(j) {
        mediaActive = !!j.id && !['done','error'].includes(j.stage);
        flash.observe(j);
        upgrade.validate();
        updateSources.validate();
        if (!j.id || j.kind !== 'upgrade') return;
        if (mediaActive && updateSources.source !== 'device') updateSources.selectDevice();
        const phase = {preparing:'正在打开源卷与镜像', checking:'正在校验升级镜像',
            writing:'正在直接写入并读回校验', done:'完成', error:'未完成'}[j.stage] || j.stage;
        message(upgrade.progress, j.image + '：' + phase + (j.message ? ' — ' + j.message : ''), j.stage === 'error');
    }

    function scheduleMediaPoll() {
        clearTimeout(mediaTimer);
        mediaTimer = setTimeout(pollMedia, mediaActive ? 1000 : 4500);
    }

    function acceptMediaJob(j) {
        ++mediaRevision;
        showMediaJob(j);
        scheduleMediaPoll();
    }

    async function pollMedia() {
        const revision = mediaRevision;
        try {
            const j = await api('media/job');
            if (revision === mediaRevision) showMediaJob(j);
        } catch (e) {
            if (mediaActive && upgrade.root.open) message(upgrade.progress, '暂时无法读取进度。', true);
        } finally {
            if (revision === mediaRevision) scheduleMediaPoll();
        }
    }
    pollMedia();
    window.rkSettings = {openNetwork, openManagement, drawFirewallTrial, firewallTrialActive:() => !!fwTrial};
})();
