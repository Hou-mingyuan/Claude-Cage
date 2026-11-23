import React, { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, Copy, ExternalLink, KeyRound, LoaderCircle, Monitor, Network, RefreshCw, Search, Server, Smartphone, Terminal, Trash2 } from "lucide-react";
import { api, writeClipboard } from "./api";
import { tailnetText } from "./tailscale-text.mjs";
import { useTranslation } from "./interface-language.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./tailscale.css";

const labels = { Running: tailnetText.connected, NeedsLogin: tailnetText.needsLogin, NeedsMachineAuth: tailnetText.needsApproval, Starting: tailnetText.connecting, Stopped: tailnetText.disconnected, NotInstalled: tailnetText.notInstalled };
const descriptions = {
  Stopped: tailnetText.joinNetwork,
  NeedsLogin: tailnetText.authorizeDevice,
  NeedsMachineAuth: tailnetText.approveDevice,
  Starting: tailnetText.connectingDetails,
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
    try { if (await onOpenLogin(value) === false && mounted.current) setLoginError(tailnetText.loginPageFailed); }
    catch { if (mounted.current) setLoginError(tailnetText.loginPageFailed); }
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
  const copy = async (text, message = tailnetText.copied) => {
    try { await writeClipboard(text); setNotice(message); }
    catch { setNotice(tailnetText.copyFailed); }
  };
  const stop = async logout => {
    if (!await onConfirm({ title: logout ? tailnetText.logoutTitle : tailnetText.disconnectTitle, body: logout ? tailnetText.logoutDetails : tailnetText.disconnectDetails, confirm: logout ? tailnetText.logout : tailnetText.disconnect })) return;
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
          <div><strong>{tr(!ready ? tailnetText.waitingForEnvironment : labels[state?.backend] || (state ? tailnetText.unconfirmedConnection : tailnetText.readingStatus))}</strong>
            {connected && <span>{state.tailnet || tr(tailnetText.privateNetwork)}</span>}
          </div>
          <button className="icon-button" aria-label={tr(tailnetText.refreshStatus)} disabled={!ready || busy} onClick={refresh}><RefreshCw size={16} /></button>
        </div>
        <p className="field-help">{tr(!ready ? tailnetText.startEnvironment : connected ? tailnetText.joinedNetwork : state?.backend === "NeedsLogin" && !state.auth_url ? tailnetText.waitingForLoginLink : descriptions[state?.backend] || tailnetText.readingTailscaleStatus)}</p>
        {state?.account && connected && <p className="tailscale-account">{tr(tailnetText.accountLabel, state.account)}</p>}
        {ready && state && <div className="tailscale-actions">
          {!state.installed ? <button className="primary-button" disabled={busy} onClick={async () => {
            setBusy(true); setError("");
            try { await onInstall(); } catch (e) { setError(e.message); }
            finally { if (mounted.current) setBusy(false); }
          }}>{tr(tailnetText.install)}</button> : <>
            {!connected && !["NeedsMachineAuth", "Starting"].includes(state.backend) && <button className="primary-button" disabled={busy} onClick={login}>{busy ? <LoaderCircle size={15} className="spin" /> : <ExternalLink size={15} />}{tr(state.enabled ? tailnetText.retryLogin : tailnetText.connectAndLogin)}</button>}
            {state.auth_url && !connected && <button className="quiet-button" disabled={busy} onClick={() => openLogin(state)}>{tr(tailnetText.openLoginPage)}</button>}
            {state.enabled && <button className="quiet-button" disabled={busy} onClick={() => stop(false)}>{tr(tailnetText.disconnect)}</button>}
            {connected && <button className="quiet-button" disabled={busy} onClick={() => stop(true)}>{tr(tailnetText.logout)}</button>}
          </>}
        </div>}
      </div>
      {(error || loginError && !connected) && <p className="error-message" role="alert">{tr(error || loginError)}</p>}
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
      {ready && state?.health?.length > 0 && <div className="tailscale-health" role="status"><strong>{tr(tailnetText.networkWarning)}</strong>{state.health.map((message, i) => <p key={i}>{tr(message)}</p>)}</div>}
      {connected && <section className="tailscale-self" aria-label={tr(tailnetText.currentDevice)}>
        <DeviceIcon os={state.os} />
        <div><strong>{state.hostname || state.name?.split(".")[0] || "Cage"} <span className="tailscale-badge">{tr(tailnetText.currentEnvironment)}</span></strong><span className="tailscale-address">{address || tr(tailnetText.allocatingAddress)}{state.os ? ` · ${state.os}` : ""}</span></div>
        {address && <button className="quiet-button" onClick={() => copy(address, tailnetText.copiedOwnAddress)}><Copy size={14} />{tr(tailnetText.copyAddress)}</button>}
      </section>}
      <section className="tailscale-section tailscale-devices" aria-labelledby="tailscale-devices-title">
        <div className="tailscale-section-heading"><h3 id="tailscale-devices-title">{tr(tailnetText.devices)}{connected && <span>{peers.length}</span>}</h3>{connected && peers.length > 0 && <span className="field-help">{tr(tailnetText.onlineCount, peers.filter(peer => peer.online).length)}</span>}</div>
        {connected && peers.length > 0 && <label className="tailscale-search"><Search size={16} /><input aria-label={tr(tailnetText.searchDevices)} type="search" placeholder={tr(tailnetText.searchNameOrAddress)} value={query} onChange={e => setQuery(e.target.value)} /></label>}
        {!connected ? <div className="tailscale-empty"><Monitor size={26} /><strong>{tr(tailnetText.devicesAfterConnection)}</strong><p>{tr(tailnetText.deviceListAfterLogin)}</p></div>
          : peers.length === 0 ? <div className="tailscale-empty"><Monitor size={26} /><strong>{tr(tailnetText.noOtherDevices)}</strong><p>{tr(tailnetText.addOtherDevices)}</p></div>
          : visible.length === 0 ? <p className="tailscale-empty">{tr(tailnetText.noMatches)}</p>
          : <div className="tailscale-peers">{visible.map(peer => <article className="tailscale-peer" key={peer.id || peer.ip}>
            <div className="tailscale-peer-row"><DeviceIcon os={peer.os} /><div className="tailscale-peer-info"><strong>{deviceName(peer)}</strong><span>{peer.ip}{peer.os ? ` · ${peer.os}` : ""}</span></div>
              <span className={`tailscale-presence${peer.online ? " online" : ""}`}><i />{tr(peer.online ? tailnetText.online : tailnetText.offline)}</span>
              <button className="icon-button" aria-label={tr(tailnetText.copyDeviceAddress, deviceName(peer))} onClick={() => copy(peer.ip, tailnetText.copiedDeviceAddress)}><Copy size={15} /></button>
              <button className="quiet-button" aria-expanded={selected === peer.ip} aria-controls={`tailnet-device-${peer.ip}`} onClick={() => { setSelected(selected === peer.ip ? "" : peer.ip); setUsername(""); setPort("80"); }}>{tr(tailnetText.useDevice)}<ChevronDown size={14} /></button>
            </div>
            {selected === peer.ip && <div className="tailscale-device-detail" id={`tailnet-device-${peer.ip}`}>
              {peer.name && <p className="field-help tailscale-dns">{peer.name}</p>}
              {!peer.online && <p className="field-help">{tr(tailnetText.deviceOffline)}</p>}
              <label className="field-label" htmlFor="tailnet-service-port">{tr(tailnetText.deviceServices)}</label>
              <p className="field-help">{tr(tailnetText.servicePortDetails)}</p>
              <div className="tailscale-connect-form"><input id="tailnet-service-port" aria-label={tr(tailnetText.servicePort)} type="number" min="1" max="65535" inputMode="numeric" value={port} onChange={e => setPort(e.target.value)} /><button className="quiet-button" disabled={busy || !Number.isInteger(Number(port)) || Number(port) < 1 || Number(port) > 65535} onClick={async () => { if (await act("open-service", { host: peer.ip, port: Number(port) })) setNotice(tailnetText.serviceReady); }}>{tr(tailnetText.createLocalAddress)}</button></div>
              {(state.forwards || []).filter(entry => entry.host === peer.ip).map(entry => <div className="tailscale-forward" key={`${entry.host}:${entry.port}`}><span><strong>{tr(tailnetText.portNumber, entry.port)}</strong><code>{entry.address}</code></span><button className="icon-button" aria-label={tr(tailnetText.copyPortAddress, entry.port)} onClick={() => copy(entry.address, tailnetText.copiedServiceAddress)}><Copy size={14} /></button><button className="quiet-button" onClick={async () => { try { if (!await onOpenBrowser(`http://${entry.address}`)) setError(tailnetText.openServiceFailed); } catch { setError(tailnetText.openServiceFailed); } }}>{tr(tailnetText.openHttp)}</button></div>)}
              <label className="field-label" htmlFor="tailnet-ssh-user"><Terminal size={14} />{tr(tailnetText.sshConnection)}</label>
              <p className="field-help">{tr(tailnetText.sshUsernameDetails)}</p>
              <div className="tailscale-connect-form"><input id="tailnet-ssh-user" value={username} onChange={e => setUsername(e.target.value)} placeholder={tr(tailnetText.sshUsername)} autoComplete="off" maxLength={128} /><button className="quiet-button" disabled={!username.trim() || !/^[a-zA-Z0-9_.@\\-]+$/.test(username.trim())} onClick={() => copy(`ssh -l '${username.trim()}' ${peer.ip}`, tailnetText.copiedSshCommand)}>{tr(tailnetText.copySshCommand)}</button></div>
              <p className="field-help">{tr(tailnetText.sshToolsDetails)}</p>
            </div>}
          </article>)}</div>}
      </section>
      {connected && <section className="tailscale-section">
        <button className="tailscale-disclosure" aria-expanded={sshOpen} aria-controls="tailnet-inbound-ssh" onClick={() => setSSHOpen(!sshOpen)}><span><KeyRound size={16} />{tr(tailnetText.inboundSsh)}</span><ChevronDown size={16} /></button>
        {sshOpen && <div id="tailnet-inbound-ssh" className="tailscale-detail">
          <p className="field-help">{tr(tailnetText.inboundSshDetails)}</p>
          {address && <div className="tailscale-command"><code>{`ssh ${state.user}@${address}`}</code><button className="icon-button" aria-label={tr(tailnetText.copyCageCommand)} onClick={() => copy(`ssh ${state.user}@${address}`)}><Copy size={15} /></button></div>}
          <label className="field-label" htmlFor="tailscale-key">{tr(tailnetText.publicKey)}</label>
          <textarea id="tailscale-key" rows={3} placeholder="ssh-ed25519 AAAA…" value={key} onChange={e => setKey(e.target.value)} maxLength={8192} spellCheck={false} />
          <button className="quiet-button" disabled={busy || !key.trim()} onClick={() => act("add-key", { key: key.trim() })}>{tr(tailnetText.addPublicKey)}</button>
          <div className="tailscale-keys">{(state.keys || []).map(entry => <div className="tailscale-key" key={entry}><code title={entry}>{entry}</code><button className="icon-button" disabled={busy} aria-label={tr(tailnetText.removePublicKey)} onClick={() => act("remove-key", { key: entry })}><Trash2 size={14} /></button></div>)}</div>
        </div>}
      </section>}
      {connected && state.proxy_url && <section className="tailscale-section">
        <button className="tailscale-disclosure" aria-expanded={appsOpen} aria-controls="tailnet-apps" onClick={() => setAppsOpen(!appsOpen)}><span><Network size={16} />{tr(tailnetText.applicationAccess)}</span><ChevronDown size={16} /></button>
        {appsOpen && <div id="tailnet-apps" className="tailscale-detail"><p className="field-help">{tr(tailnetText.applicationAccessDetails)}</p><button className="quiet-button" onClick={() => copy(state.proxy_url, tailnetText.copiedNetworkProxy)}>{tr(tailnetText.copyNetworkProxy)}</button><p className="field-help">{tr(tailnetText.networkProxyDetails)}</p></div>}
      </section>}
      <p className="field-help tailscale-footer">{tr(tailnetText.accessRules)}</p>
      {notice && <p className="tailscale-notice" role="status"><Check size={14} />{tr(notice)}</p>}
    </div>;
}
