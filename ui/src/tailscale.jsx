import React, { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, Copy, ExternalLink, KeyRound, LoaderCircle, Monitor, Network, RefreshCw, Search, Server, Smartphone, Terminal, Trash2 } from "lucide-react";
import { api, writeClipboard } from "./api";
import { useTranslation } from "./interface-language.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./tailscale.css";

const labels = { Running: "已连接", NeedsLogin: "等待登录", NeedsMachineAuth: "等待管理员批准", Starting: "正在连接", Stopped: "已断开", NotInstalled: "尚未安装" };
const descriptions = {
  Stopped: "登录 Tailscale，让 Cage 加入你的私网。其他设备加入同一网络后，会显示在这里。",
  NeedsLogin: "在内置浏览器中登录并授权当前设备。",
  NeedsMachineAuth: "登录已完成。请让此网络的管理员在 Tailscale 管理台批准 Cage 设备。",
  Starting: "正在连接 Tailscale，设备清单会在连接成功后自动更新。",
};
const deviceName = peer => peer.hostname || peer.name?.split(".")[0] || peer.ip;
function DeviceIcon({ os }) { return ["android", "iOS"].includes(os) ? <Smartphone size={20} /> : <Monitor size={20} />; }

export function TailscalePanel({ status, onOpenLogin, onOpenBrowser, onInstall, onConfirm }) {
  const tr = useTranslation();
  const windowVisible = useWindowVisibility();
  const [state, setState] = useState(null), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const [loginError, setLoginError] = useState("");
  const [key, setKey] = useState(""), [notice, setNotice] = useState(""), [query, setQuery] = useState("");
  const [sshOpen, setSSHOpen] = useState(false), [selected, setSelected] = useState(""), [username, setUsername] = useState("");
  const [port, setPort] = useState("80"), [appsOpen, setAppsOpen] = useState(false);
  const [controlOpen, setControlOpen] = useState(false), [controlURL, setControlURL] = useState("");
  const lastControlURL = useRef(undefined);
  const mounted = useRef(true), inflight = useRef(false), loginRequested = useRef(false), revision = useRef(0);
  const ready = status.ready && !status.job?.running;
  const connected = ready && state?.backend === "Running";
  const address = state?.ips?.find(ip => !ip.includes(":"));
  const peers = [...(state?.peers || [])].sort((a, b) => Number(b.online) - Number(a.online) || deviceName(a).localeCompare(deviceName(b)));
  const visible = peers.filter(peer => [deviceName(peer), peer.name, peer.ip, peer.os].some(value => value?.toLowerCase().includes(query.trim().toLowerCase())));
  const syncControlURL = value => {
    const server = value?.control_url || "";
    if (lastControlURL.current !== server) {
      lastControlURL.current = server;
      setControlURL(server);
    }
  };
  const openLogin = async value => {
    loginRequested.current = false;
    setLoginError("");
    try { if (await onOpenLogin(value) === false && mounted.current) setLoginError("登录页面未能打开，请点击“打开登录页面”重试。"); }
    catch { if (mounted.current) setLoginError("登录页面未能打开，请点击“打开登录页面”重试。"); }
  };
  const refresh = async () => {
    if (inflight.current || !ready) return;
    inflight.current = true;
    const current = ++revision.current;
    try {
      const value = await api("/tailscale");
      if (mounted.current && current === revision.current) {
        setState(value); setError(""); syncControlURL(value);
        if (loginRequested.current && value.auth_url) await openLogin(value);
        if (value.backend === "Running") loginRequested.current = false;
      }
      return value;
    } catch (e) { if (mounted.current && current === revision.current) setError(e.message); }
    finally { if (current === revision.current) inflight.current = false; }
  };
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; revision.current++; inflight.current = false; loginRequested.current = false; };
  }, []);
  useEffect(() => {
    if (!ready || !windowVisible) return;
    void refresh();
    const timer = setInterval(() => void refresh(), 3000);
    return () => { clearInterval(timer); };
  }, [ready, windowVisible]);
  const act = async (action, body = {}) => {
    if (busy) return;
    setBusy(true); setError(""); setLoginError(""); setNotice(""); revision.current++;
    try {
      inflight.current = true;
      const value = await api(`/tailscale/${action}`, body);
      if (mounted.current) { setState(value); syncControlURL(value); if (action === "add-key") setKey(""); }
      return value;
    } catch (e) { if (mounted.current) setError(e.message); }
    finally { inflight.current = false; if (mounted.current) setBusy(false); }
  };
  const login = async () => {
    loginRequested.current = true;
    const result = await act(state?.enabled ? "login" : "connect");
    if (mounted.current && result?.auth_url) await openLogin(result);
    else if (!result || result.backend === "Running") loginRequested.current = false;
  };
  const copy = async (text, message = "已复制") => {
    try { await writeClipboard(text); setNotice(message); }
    catch { setNotice("复制失败，请选中文本手动复制。"); }
  };
  const stop = async logout => {
    if (!await onConfirm({ title: logout ? "退出 Tailscale 登录？" : "断开 Tailscale？", body: logout ? "私网连接会中断，下次连接需要重新登录。已添加的 SSH 公钥保留。" : "正在使用的私网连接会中断，登录状态和 SSH 公钥保留。", confirm: logout ? "退出登录" : "断开连接" })) return;
    loginRequested.current = false;
    if (await act(logout ? "logout" : "disconnect")) { setSelected(""); setSSHOpen(false); }
  };
  const saveControlServer = async nextURL => {
    const trimmed = nextURL.trim();
    const current = state?.control_url || "";
    if (state?.enabled && trimmed !== current) {
      if (!await onConfirm({
        title: tr("切换控制服务器？"),
        body: tr("当前私网连接会断开，需要重新登录目标服务器。官方服务与各处自建服务器的设备身份和 SSH 公钥分别保留；切换不会在原服务器上注销本机，它仍留在原服务器的设备列表中。如需在原服务器上注销本机，请先取消，在当前服务器点击「退出登录」后再切换。"),
        confirm: tr("保存并切换"),
      })) return;
    }
    loginRequested.current = false;
    const result = await act("control-server", { control_url: trimmed });
    if (mounted.current && result) {
      setSelected("");
      setSSHOpen(false);
      lastControlURL.current = result.control_url || "";
      setControlURL(result.control_url || "");
      setNotice(result.control_url ? tr("已保存自建控制服务器地址") : tr("已恢复使用 Tailscale 官方控制服务器"));
    }
  };
  return <div className="tailscale-panel">
      <div className={`tailscale-overview${connected ? " connected" : ""}`}>
        <div className="tailscale-state" role="status">
          <div className="tailscale-network-icon"><Network size={24} /></div>
          <div><strong>{!ready ? "等待 Cage 环境就绪" : labels[state?.backend] || (state ? "连接状态待确认" : "正在读取状态…")}</strong>
            {connected && <span>{state.tailnet || "Tailscale 私网"}</span>}
          </div>
          <button className="icon-button" aria-label="刷新 Tailscale 状态" disabled={!ready || busy} onClick={refresh}><RefreshCw size={16} /></button>
        </div>
        <p className="field-help">{!ready ? "启动 Cage 环境后，即可登录并连接私网。" : connected ? "Cage 已加入私网。选择设备，查看地址并使用它的服务。" : state?.backend === "NeedsLogin" && !state.auth_url ? "正在向 Tailscale 获取登录链接。若长时间没有打开浏览器，请检查环境联网后重新发起登录。" : descriptions[state?.backend] || "正在读取当前环境的 Tailscale 状态。"}</p>
        {state?.account && connected && <p className="tailscale-account">登录账号 · {state.account}</p>}
        {ready && state && <div className="tailscale-actions">
          {!state.installed ? <button className="primary-button" disabled={busy} onClick={async () => {
            setBusy(true); setError("");
            try { await onInstall(); } catch (e) { setError(e.message); }
            finally { if (mounted.current) setBusy(false); }
          }}>安装 Tailscale</button> : <>
            {!connected && !["NeedsMachineAuth", "Starting"].includes(state.backend) && <button className="primary-button" disabled={busy} onClick={login}>{busy ? <LoaderCircle size={15} className="spin" /> : <ExternalLink size={15} />}{state.enabled ? "重新发起登录" : "连接并登录"}</button>}
            {state.auth_url && !connected && <button className="quiet-button" disabled={busy} onClick={() => openLogin(state)}>打开登录页面</button>}
            {state.enabled && <button className="quiet-button" disabled={busy} onClick={() => stop(false)}>断开连接</button>}
            {connected && <button className="quiet-button" disabled={busy} onClick={() => stop(true)}>退出登录</button>}
          </>}
        </div>}
      </div>
      {(error || loginError && !connected) && <p className="error-message" role="alert">{error || loginError}</p>}
      <section className="tailscale-section">
        <button className="tailscale-disclosure" aria-expanded={controlOpen} aria-controls="tailnet-control-server" onClick={() => setControlOpen(!controlOpen)}>
          <span><Server size={16} />{tr("控制服务器")} · {state?.control_url || tr("Tailscale 官方")}</span>
          <ChevronDown size={16} />
        </button>
        {controlOpen && <div id="tailnet-control-server" className="tailscale-detail">
          <p className="field-help">{tr("留空使用 Tailscale 官方服务；如需接入自建 Headscale，请填写 HTTPS 根地址（例如 https://headscale.example.com）。各控制服务器的设备身份分别保存在独立目录，切换不会覆盖原有身份。")}</p>
          <label className="field-label" htmlFor="tailscale-control-url">{tr("自建控制服务器地址（HTTPS，留空为官方）")}</label>
          <div className="tailscale-connect-form">
            <input
              id="tailscale-control-url"
              type="url"
              value={controlURL}
              onChange={e => setControlURL(e.target.value)}
              placeholder="https://headscale.example.com"
              spellCheck={false}
              autoComplete="off"
              maxLength={256}
            />
            <button className="quiet-button" disabled={busy || controlURL.trim() === (state?.control_url || "")} onClick={() => saveControlServer(controlURL)}>
              {state?.enabled ? tr("保存并切换") : tr("保存")}
            </button>
            {state?.control_url && <button className="quiet-button" disabled={busy} onClick={() => saveControlServer("")}>
              {tr("恢复官方服务")}
            </button>}
          </div>
        </div>}
      </section>
      {ready && state?.health?.length > 0 && <div className="tailscale-health" role="status"><strong>网络需要留意</strong>{state.health.map((message, i) => <p key={i}>{message}</p>)}</div>}
      {connected && <section className="tailscale-self" aria-label="当前 Cage 设备">
        <DeviceIcon os={state.os} />
        <div><strong>{state.hostname || state.name?.split(".")[0] || "Cage"} <span className="tailscale-badge">当前环境</span></strong><span className="tailscale-address">{address || "正在分配地址…"}{state.os ? ` · ${state.os}` : ""}</span></div>
        {address && <button className="quiet-button" onClick={() => copy(address, "已复制 Cage 私网地址")}><Copy size={14} />复制地址</button>}
      </section>}
      <section className="tailscale-section tailscale-devices" aria-labelledby="tailscale-devices-title">
        <div className="tailscale-section-heading"><h3 id="tailscale-devices-title">网络中的设备{connected && <span>{peers.length}</span>}</h3>{connected && peers.length > 0 && <span className="field-help">{peers.filter(peer => peer.online).length} 台在线</span>}</div>
        {connected && peers.length > 0 && <label className="tailscale-search"><Search size={16} /><input aria-label="搜索设备" type="search" placeholder="搜索设备名称或地址" value={query} onChange={e => setQuery(e.target.value)} /></label>}
        {!connected ? <div className="tailscale-empty"><Monitor size={26} /><strong>连接后查看你的设备</strong><p>在浏览器完成登录，设备清单会自动出现。</p></div>
          : peers.length === 0 ? <div className="tailscale-empty"><Monitor size={26} /><strong>私网已连接，还没有发现其他设备</strong><p>在电脑或手机安装 Tailscale，并加入同一网络。这里只显示此网络允许 Cage 发现的设备。</p></div>
          : visible.length === 0 ? <p className="tailscale-empty">没有匹配的设备，请尝试其他名称或地址。</p>
          : <div className="tailscale-peers">{visible.map(peer => <article className="tailscale-peer" key={peer.id || peer.ip}>
            <div className="tailscale-peer-row"><DeviceIcon os={peer.os} /><div className="tailscale-peer-info"><strong>{deviceName(peer)}</strong><span>{peer.ip}{peer.os ? ` · ${peer.os}` : ""}</span></div>
              <span className={`tailscale-presence${peer.online ? " online" : ""}`}><i />{peer.online ? "在线" : "离线"}</span>
              <button className="icon-button" aria-label={`复制 ${deviceName(peer)} 的地址`} onClick={() => copy(peer.ip, "已复制设备私网地址")}><Copy size={15} /></button>
              <button className="quiet-button" aria-expanded={selected === peer.ip} aria-controls={`tailnet-device-${peer.ip}`} onClick={() => { setSelected(selected === peer.ip ? "" : peer.ip); setUsername(""); setPort("80"); }}>使用设备<ChevronDown size={14} /></button>
            </div>
            {selected === peer.ip && <div className="tailscale-device-detail" id={`tailnet-device-${peer.ip}`}>
              {peer.name && <p className="field-help tailscale-dns">{peer.name}</p>}
              {!peer.online && <p className="field-help">设备当前离线，请先在该设备上连接 Tailscale。</p>}
              <label className="field-label" htmlFor="tailnet-service-port">访问设备上的服务</label>
              <p className="field-help">填写服务端口，为 Cage 内的浏览器、数据库客户端或其他应用创建本地连接地址。</p>
              <div className="tailscale-connect-form"><input id="tailnet-service-port" aria-label="服务端口" type="number" min="1" max="65535" inputMode="numeric" value={port} onChange={e => setPort(e.target.value)} /><button className="quiet-button" disabled={busy || !Number.isInteger(Number(port)) || Number(port) < 1 || Number(port) > 65535} onClick={async () => { if (await act("open-service", { host: peer.ip, port: Number(port) })) setNotice("本地连接地址已就绪，请在 Cage 环境内使用。"); }}>创建连接地址</button></div>
              {(state.forwards || []).filter(entry => entry.host === peer.ip).map(entry => <div className="tailscale-forward" key={`${entry.host}:${entry.port}`}><span><strong>端口 {entry.port}</strong><code>{entry.address}</code></span><button className="icon-button" aria-label={`复制端口 ${entry.port} 的连接地址`} onClick={() => copy(entry.address, "已复制 Cage 内可用的连接地址")}><Copy size={14} /></button><button className="quiet-button" onClick={async () => { try { if (!await onOpenBrowser(`http://${entry.address}`)) setError("服务页面未能打开，请重试。"); } catch { setError("服务页面未能打开，请重试。"); } }}>打开 HTTP 页面</button></div>)}
              <label className="field-label" htmlFor="tailnet-ssh-user"><Terminal size={14} />SSH 连接</label>
              <p className="field-help">填写这台设备的系统用户名。目标设备需开启 SSH，并允许当前账号登录。</p>
              <div className="tailscale-connect-form"><input id="tailnet-ssh-user" value={username} onChange={e => setUsername(e.target.value)} placeholder="设备的系统用户名" autoComplete="off" maxLength={128} /><button className="quiet-button" disabled={!username.trim() || !/^[a-zA-Z0-9_.@\\-]+$/.test(username.trim())} onClick={() => copy(`ssh -l '${username.trim()}' ${peer.ip}`, "已复制 SSH 命令，请在 Cage 终端运行")}>复制 SSH 命令</button></div>
              <p className="field-help">SSH、SFTP 与使用 OpenSSH 的工具可直接使用以上私网地址。其他应用的接入方式见下方“应用访问私网”。</p>
            </div>}
          </article>)}</div>}
      </section>
      {connected && <section className="tailscale-section">
        <button className="tailscale-disclosure" aria-expanded={sshOpen} aria-controls="tailnet-inbound-ssh" onClick={() => setSSHOpen(!sshOpen)}><span><KeyRound size={16} />从其他设备 SSH 到 Cage</span><ChevronDown size={16} /></button>
        {sshOpen && <div id="tailnet-inbound-ssh" className="tailscale-detail">
          <p className="field-help">仅在需要远程终端、SFTP 或编辑器连接时配置。将连接设备的 SSH 公钥添加到这里，私钥保留在原设备。</p>
          {address && <div className="tailscale-command"><code>{`ssh ${state.user}@${address}`}</code><button className="icon-button" aria-label="复制连接 Cage 命令" onClick={() => copy(`ssh ${state.user}@${address}`)}><Copy size={15} /></button></div>}
          <label className="field-label" htmlFor="tailscale-key">连接设备的 SSH 公钥</label>
          <textarea id="tailscale-key" rows={3} placeholder="ssh-ed25519 AAAA…" value={key} onChange={e => setKey(e.target.value)} maxLength={8192} spellCheck={false} />
          <button className="quiet-button" disabled={busy || !key.trim()} onClick={() => act("add-key", { key: key.trim() })}>添加公钥</button>
          <div className="tailscale-keys">{(state.keys || []).map(entry => <div className="tailscale-key" key={entry}><code title={entry}>{entry}</code><button className="icon-button" disabled={busy} aria-label="移除 SSH 公钥" onClick={() => act("remove-key", { key: entry })}><Trash2 size={14} /></button></div>)}</div>
        </div>}
      </section>}
      {connected && state.proxy_url && <section className="tailscale-section">
        <button className="tailscale-disclosure" aria-expanded={appsOpen} aria-controls="tailnet-apps" onClick={() => setAppsOpen(!appsOpen)}><span><Network size={16} />应用访问私网</span><ChevronDown size={16} /></button>
        {appsOpen && <div id="tailnet-apps" className="tailscale-detail"><p className="field-help">需要直接使用设备域名、HTTPS 或多个端口时，在 Cage 内的应用中配置 HTTP 代理。仅允许访问此私网中可见的设备，公网继续使用原有出口。</p><button className="quiet-button" onClick={() => copy(state.proxy_url, "已复制私网 HTTP 代理地址（含本地访问凭据）")}>复制私网代理地址</button><p className="field-help">代理地址含本地访问凭据，请仅用于 Cage 内的应用。此接入方式支持 TCP 服务；UDP、系统 VPN 和子网路由未启用。</p></div>}
      </section>}
      <p className="field-help tailscale-footer">其他设备需安装 Tailscale 并加入同一网络。设备访问受网络权限控制；暂停 Cage 联网或停止环境会中断私网连接。</p>
      {notice && <p className="tailscale-notice" role="status"><Check size={14} />{notice}</p>}
    </div>;
}
