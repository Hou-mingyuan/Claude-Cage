// The server is authoritative for the site, action and one-time redemption.
let library;
async function loadWidget(region) {
  if (!library) {
    library = new Promise((resolve, reject) => {
      const script = document.createElement('script');
      const domain = region === 'china' ? 'challenges.cloudflare-cn.com' : 'challenges.cloudflare.com';
      const fail = () => {
        clearTimeout(timer);
        delete window.tokenLinkProofLoaded;
        script.remove();
        library = undefined;
        reject(new Error('未能加载安全验证，请检查网络后重试。'));
      };
      const timer = setTimeout(fail, 15000);
      script.onerror = fail;
      window.tokenLinkProofLoaded = () => {
        if (!window.turnstile) return fail();
        clearTimeout(timer);
        delete window.tokenLinkProofLoaded;
        resolve(window.turnstile);
      };
      script.src = 'https://' + domain + '/turnstile/v0/api.js?render=explicit&onload=tokenLinkProofLoaded';
      document.head.append(script);
    });
  }
  return library;
}

export async function pageProof(configURL, action) {
  if (window.top !== window.self) throw new Error('请在正式站点的独立页面使用答疑。');
  const res = await fetch(configURL, {cache:'no-store', signal:AbortSignal.timeout(12000)});
  if (!res.ok) throw new Error('暂时无法读取验证配置，请刷新后重试。');
  const config = await res.json();
  if (config.enabled === false) return '';
  if (config.enabled !== true || !config.siteKey) throw new Error('安全验证暂不可用。');
  const widget = await loadWidget(config.region);
  const dialog = document.createElement('dialog');
  const label = document.createElement('p');
  label.textContent = '正在验证本次请求';
  dialog.setAttribute('aria-label', label.textContent);
  const box = document.createElement('div');
  const cancel = document.createElement('button');
  cancel.type = 'button'; cancel.textContent = '取消';
  dialog.append(label,box,cancel);
  document.body.append(dialog);
  let id;
  try {
    dialog.showModal();
    return await new Promise((resolve, reject) => {
      const fail = () => reject(new Error('安全验证未完成，请重试。'));
      const timer = setTimeout(fail,120000);
      dialog.addEventListener('close',()=>clearTimeout(timer),{once:true});
      cancel.onclick = fail; dialog.oncancel = fail;
      id = widget.render(box,{sitekey:config.siteKey,action,theme:'auto',size:'flexible',
        callback:resolve,'error-callback':fail,'expired-callback':fail,'timeout-callback':fail});
    });
  } finally {
    if (id !== undefined) widget.remove(id);
    dialog.close(); dialog.remove();
  }
}
