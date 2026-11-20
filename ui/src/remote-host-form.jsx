import React, { useState } from "react";
import { api } from "./api";
import { InlineSelect } from "./inline-select";
import { Modal } from "./modal";
import { useTranslation } from "./interface-language.mjs";

const emptyHost = () => ({
  name: "",
  host: "",
  port: 22,
  user: "",
  platform: "linux",
  auth: { kind: "key_file", identity_file: "" },
  executable: "cage",
  config_path: "",
});

export function HostForm({ initial, platform, onSaved, onClose }) {
  const tr = useTranslation();
  const [form, setForm] = useState(() =>
    initial ? structuredClone(initial) : emptyHost(),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const update = (field, value) =>
    setForm((old) => ({
      ...old,
      [field]: value,
      ...(["host", "port"].includes(field) ? { host_key: undefined } : {}),
    }));
  async function save(event) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const saved = await api(
        form.id ? `/client/hosts/${form.id}` : "/client/hosts",
        { ...form, port: Number(form.port) },
        form.id ? "PUT" : "POST",
      );
      await onSaved(saved);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Modal
      title={form.id ? tr("编辑远程主机") : tr("添加远程主机")}
      onClose={busy ? undefined : onClose}
    >
      <form className="remote-host-form" onSubmit={save}>
        {error && (
          <p className="error-message" role="alert">
            {error}
          </p>
        )}
        <fieldset disabled={busy}>
          <label>
            {tr("主机名称")}
            <input
              required
              value={form.name}
              onChange={(event) => update("name", event.target.value)}
              placeholder={tr("例如：东京开发机")}
              maxLength={200}
            />
          </label>
          <div className="remote-form-row">
            <label>
              {tr("服务器地址")}
              <input
                required
                value={form.host}
                onChange={(event) => update("host", event.target.value)}
                placeholder={tr("IP 地址或域名")}
                autoComplete="off"
                spellCheck="false"
              />
            </label>
            <label className="remote-port">
              {tr("SSH 端口")}
              <input
                required
                type="number"
                min="1"
                max="65535"
                value={form.port}
                onChange={(event) => update("port", event.target.value)}
              />
            </label>
          </div>
          <label>
            {tr("远程系统")}
            <InlineSelect
              inline
              aria-label={tr("远程系统")}
              value={form.platform || "linux"}
              onValueChange={(value) => update("platform", value)}
            >
              <option value="linux">Linux</option>
              <option value="windows">{tr("Windows（已部署服务）")}</option>
            </InlineSelect>
          </label>
          <label>
            {tr("SSH 用户名")}
            <input
              required
              value={form.user}
              onChange={(event) => update("user", event.target.value)}
              placeholder="cage"
              autoComplete="off"
              spellCheck="false"
            />
          </label>
          <label>
            {tr("SSH 登录方式")}
            <InlineSelect
              aria-label={tr("SSH 登录方式")}
              value={form.auth.kind}
              onValueChange={(kind) =>
                update("auth", {
                  kind,
                  identity_file:
                    kind === "key_file" ? form.auth.identity_file || "" : "",
                })
              }
            >
              <option value="key_file">{tr("本机密钥文件")}</option>
              <option value="agent">{tr("本机 SSH agent")}</option>
            </InlineSelect>
          </label>
          {form.auth.kind === "key_file" && (
            <label>
              {tr("本机私钥文件")}
              <input
                aria-label={tr("本机私钥文件")}
                required
                value={form.auth.identity_file}
                onChange={(event) =>
                  update("auth", {
                    ...form.auth,
                    identity_file: event.target.value,
                  })
                }
                placeholder={
                  platform === "windows"
                    ? tr("C:\\Users\\你的名字\\.ssh\\id_ed25519")
                    : tr("/Users/你的名字/.ssh/id_ed25519")
                }
                autoComplete="off"
                spellCheck="false"
              />
              <small>{tr("填写绝对路径。密钥留在本机，不上传到云端。")}</small>
            </label>
          )}
          <details>
            <summary>{tr("云端安装路径")}</summary>
            <div className="remote-advanced">
              <label>
                {tr("Cage 程序路径")}
                <input
                  value={form.executable || ""}
                  onChange={(event) => update("executable", event.target.value)}
                  placeholder="cage"
                  spellCheck="false"
                />
                <small>{tr("默认使用 cage，也可填写云端程序的绝对路径。")}</small>
              </label>
              <label>
                {tr("Cage 配置文件")}
                <input
                  value={form.config_path || ""}
                  onChange={(event) =>
                    update("config_path", event.target.value)
                  }
                  placeholder={form.platform === "windows" ? tr("例如 C:\\ProgramData\\Cage Remote\\config.json") : tr("默认使用云端账户的 ~/.claude-cage/config.json")}
                  spellCheck="false"
                />
                <small>{tr("自定义时填写云端绝对路径。")}</small>
              </label>
            </div>
          </details>
          <div className="remote-button-row">
            <button className="primary" type="submit">
              {tr("保存主机")}
            </button>
            <button type="button" onClick={onClose}>
              {tr("取消")}
            </button>
          </div>
        </fieldset>
      </form>
    </Modal>
  );
}
