import { useCallback, useEffect, useState } from "react";

import {
  UnauthorizedError,
  createPromptTemplate,
  deletePromptTemplate,
  getSettings,
  getSystemInfo,
  listPromptTemplates,
  rotateAdminToken,
  saveSettings,
  setAdminToken,
  updatePromptTemplate,
} from "../api";
import { useConfirm, useToast } from "../components/feedback";
import { useDialog } from "../useDialog";
import {
  APPEARANCE_EVENT,
  BUILTIN_THEMES,
  THEME_LABELS,
  THEME_PACKS,
  applyDensity,
  applyTheme,
  currentDensity,
  currentTheme,
} from "../theme";
import { formatTimestamp } from "../logFormat";
import type { PromptTemplate, RuntimeSettings, SystemInfo } from "../types";

/** 和 App 的路由共用一个键；改完下次打开控制台就落在这一页。 */
const DEFAULT_HOME_KEY = "wildtoken_default_home";

function readDefaultHome(): string {
  try {
    return localStorage.getItem(DEFAULT_HOME_KEY) ?? "dashboard";
  } catch {
    return "dashboard";
  }
}

/** 每张卡管的字段。保存只提交本卡的，其余取服务端最近一次的值：原先整页
    提交，另一张卡上没确认的改动跟着落库。 */
const CARD_FIELDS = {
  log: ["log_body_keep_count", "log_retention_days", "log_body_max_bytes", "image_storage_max_mb"],
  routing: [
    "max_retries",
    "same_upstream_retry_interval_ms",
    "auto_weight_failure_penalty",
    "auto_weight_success_increment",
    "auto_weight_recovery_increment",
    "auto_weight_recovery_interval_seconds",
  ],
  proxy: ["proxy_enabled", "proxy_url"],
  timeout: ["default_upstream_timeout_seconds"],
  dashboard: ["dashboard_multiplier"],
} satisfies Record<string, (keyof RuntimeSettings)[]>;

type SettingsCard = keyof typeof CARD_FIELDS;

/** base 换上 source 里 keys 的值。 */
function withFields(
  base: RuntimeSettings,
  source: RuntimeSettings,
  keys: readonly (keyof RuntimeSettings)[],
): RuntimeSettings {
  const next: Record<string, unknown> = { ...base };
  for (const key of keys) next[key] = source[key];
  return next as unknown as RuntimeSettings;
}

function formatCount(value: number): string {
  return Number(value || 0).toLocaleString("zh-CN");
}

function formatBytes(value: number | null | undefined): string {
  if (typeof value !== "number" || !Number.isFinite(value)) return "—";
  if (value >= 1024 * 1024 * 1024) return `${(value / 1024 / 1024 / 1024).toFixed(1)}GB`;
  if (value >= 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)}MB`;
  if (value >= 1024) return `${Math.round(value / 1024)}KB`;
  return `${value}B`;
}

function formatUptime(seconds: number | null): string {
  if (seconds === null || !Number.isFinite(seconds)) return "—";
  const total = Math.max(0, Math.floor(seconds));
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = total % 60;
  if (days > 0) return `${days} 天 ${hours} 小时 ${minutes} 分`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分 ${rest} 秒`;
  if (minutes > 0) return `${minutes} 分 ${rest} 秒`;
  return `${rest} 秒`;
}

function formatDuration(ms: number | null | undefined): string {
  if (typeof ms !== "number" || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(ms < 10_000 ? 1 : 0).replace(/\.0$/, "")}s`;
}

export function SettingsPage({ onUnauthorized }: { onUnauthorized: (message: string) => void }) {
  const [settings, setSettings] = useState<RuntimeSettings | null>(null);
  /* 服务端最近一次返回的设置。保存以它为底，只换上当前那张卡的字段。 */
  const [saved, setSaved] = useState<RuntimeSettings | null>(null);
  const [templates, setTemplates] = useState<PromptTemplate[]>([]);
  const [system, setSystem] = useState<SystemInfo | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  /* 哪一张卡正在保存，标出具体那张，免得点了日志策略却看到代理那边转圈。 */
  const [savingCard, setSavingCard] = useState<SettingsCard | null>(null);
  const [theme, setTheme] = useState(currentTheme);
  const [density, setDensity] = useState(currentDensity);
  const [editingTemplate, setEditingTemplate] = useState<{ template: PromptTemplate | null } | null>(
    null,
  );
  const [rotatedToken, setRotatedToken] = useState("");
  const [rotating, setRotating] = useState(false);
  const [defaultHome, setDefaultHome] = useState(readDefaultHome);
  const [refreshingSystem, setRefreshingSystem] = useState(false);

  const toast = useToast();
  const confirm = useConfirm();

  const reload = useCallback(async () => {
    try {
      const [loaded, loadedTemplates, loadedSystem] = await Promise.all([
        getSettings(),
        listPromptTemplates(),
        getSystemInfo(),
      ]);
      setSettings(loaded);
      setSaved(loaded);
      setTemplates(loadedTemplates);
      setSystem(loadedSystem);
      setError("");
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [onUnauthorized]);

  useEffect(() => {
    void reload();
  }, [reload]);

  // 顶栏也能改外观，跟着广播重读，两处显示的不会分叉。
  useEffect(() => {
    const sync = () => {
      setTheme(currentTheme());
      setDensity(currentDensity());
    };
    window.addEventListener(APPEARANCE_EVENT, sync);
    return () => window.removeEventListener(APPEARANCE_EVENT, sync);
  }, []);

  function patch<K extends keyof RuntimeSettings>(key: K, value: RuntimeSettings[K]) {
    setSettings((current) => (current ? { ...current, [key]: value } : current));
  }

  async function save(card: SettingsCard) {
    if (!settings || !saved) return;
    setSavingCard(card);
    try {
      /* revision 取服务端那份原样带回去：后端靠它拒掉过期的写入。 */
      const result = await saveSettings(withFields(saved, settings, CARD_FIELDS[card]));
      setSaved(result);
      // 本卡换成服务端的结果，别的卡上没保存的草稿原样留着。
      setSettings((current) =>
        current
          ? {
              ...withFields(current, result, CARD_FIELDS[card]),
              revision: result.revision,
              updated_at: result.updated_at,
            }
          : result,
      );
      toast("设置已保存。", { tone: "ok" });
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else toast(`保存失败：${err instanceof Error ? err.message : String(err)}`, { tone: "error" });
    } finally {
      setSavingCard(null);
    }
  }

  async function refreshSystem() {
    setRefreshingSystem(true);
    try {
      setSystem(await getSystemInfo());
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else toast(`读取运行信息失败：${err instanceof Error ? err.message : String(err)}`, { tone: "error" });
    } finally {
      setRefreshingSystem(false);
    }
  }

  async function rotate(token: string) {
    setRotating(false);
    try {
      await rotateAdminToken(token);
      // 当前页面接着用新令牌，否则下一个请求就 401。
      setAdminToken(token);
      setRotatedToken(token);
      toast("管理员令牌已更换，当前控制台已改用新令牌。", { tone: "warn", durationMs: 9000 });
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else toast(`更换失败：${err instanceof Error ? err.message : String(err)}`, { tone: "error" });
    }
  }

  function switchTheme(next: string) {
    applyTheme(next);
    setTheme(next);
  }

  function switchDensity(next: string) {
    applyDensity(next);
    setDensity(next);
  }

  function switchDefaultHome(next: string) {
    setDefaultHome(next);
    try {
      localStorage.setItem(DEFAULT_HOME_KEY, next);
    } catch {
      // 存不进去下次打开记不住，不影响当前页面。
    }
  }

  async function saveTemplate(name: string, prompt: string) {
    const target = editingTemplate?.template;
    try {
      if (target) await updatePromptTemplate(target.id, { name, prompt });
      else await createPromptTemplate({ name, prompt });
      setEditingTemplate(null);
      setTemplates(await listPromptTemplates());
      toast(`Prompt「${name}」已${target ? "保存" : "创建"}。`, { tone: "ok" });
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else toast(`保存失败：${err instanceof Error ? err.message : String(err)}`, { tone: "error" });
    }
  }

  async function removeTemplate(template: PromptTemplate) {
    const ok = await confirm({
      title: "删除 Prompt？",
      message: `「${template.name}」将被删除。`,
      confirmLabel: "删除",
    });
    if (!ok) return;
    try {
      await deletePromptTemplate(template.id);
      setTemplates(await listPromptTemplates());
      toast(`Prompt「${template.name}」已删除。`, { tone: "ok" });
    } catch (err) {
      if (err instanceof UnauthorizedError) onUnauthorized(err.message);
      else toast(`删除失败：${err instanceof Error ? err.message : String(err)}`, { tone: "error" });
    }
  }

  const metrics = system?.runtime_metrics;
  const cleanup = metrics?.cleanup;

  return (
    <section className="view settings-view" data-view="settings">
      <section className="panel settings-hero">
        <div className="panel-head">
          <div>
            <span className="eyebrow">CONSOLE CONTROL</span>
            <h2>设置</h2>
            <p>将本机偏好与网关运行策略分别管理；敏感凭证不会在此读取或保存。</p>
          </div>
        </div>

        {error ? (
          <p className="settings-inline-status" role="alert">
            {error}
          </p>
        ) : null}

        <div className="settings-stack">
          <section className="settings-card">
            <div className="settings-card-head">
              <div>
                <h3>控制台偏好</h3>
                <p>仅保存在当前浏览器，并立即生效。</p>
              </div>
              <span className="settings-local-tag">本地</span>
            </div>
            <div className="settings-preferences-grid">
              <fieldset className="settings-choice-group settings-theme-choice-group">
                <legend>主题</legend>
                <select
                  aria-label="主题"
                  value={theme}
                  onChange={(event) => switchTheme(event.target.value)}
                >
                  {[...BUILTIN_THEMES, ...Object.keys(THEME_PACKS)].map((id) => (
                    <option key={id} value={id}>
                      {THEME_LABELS[id] ?? id}
                    </option>
                  ))}
                </select>
                <span className="field-hint">主题与旧控制台共用同一份设置。</span>
              </fieldset>

              {/* 旧版还有一项「日志自动刷新」。新版日志页走 SSE 推送，没有轮询
                  间隔可调，放一个控制不了任何东西的下拉不如不放。 */}
              <label className="field">
                <span className="field-label">默认首页</span>
                <select
                  aria-label="默认首页"
                  value={defaultHome}
                  onChange={(event) => switchDefaultHome(event.target.value)}
                >
                  <option value="dashboard">看板</option>
                  <option value="upstreams">渠道</option>
                  <option value="logs">日志</option>
                  <option value="tokens">令牌</option>
                  <option value="groups">分组</option>
                  <option value="debug">调试</option>
                  <option value="images">生图</option>
                  <option value="settings">设置</option>
                </select>
                <span className="field-hint">地址栏带着页面锚点时，仍优先进那一页。</span>
              </label>

              <fieldset className="settings-choice-group">
                <legend>显示密度</legend>
                <div className="segmented-control" aria-label="显示密度">
                  <button
                    type="button"
                    data-density-choice="comfortable"
                    aria-pressed={density === "comfortable"}
                    onClick={() => switchDensity("comfortable")}
                  >
                    舒适
                  </button>
                  <button
                    type="button"
                    data-density-choice="compact"
                    aria-pressed={density === "compact"}
                    onClick={() => switchDensity("compact")}
                  >
                    紧凑
                  </button>
                </div>
              </fieldset>
            </div>
          </section>

          {loading || !settings ? (
            <p className="settings-loading">加载中…</p>
          ) : (
            <>
              <section className="settings-card">
                <div className="settings-card-head">
                  <div>
                    <h3>日志与存储</h3>
                    <p>
                      这些策略由服务端保存，保存后用于新的快照；正文清理通常在一分钟内生效，过期日志按小时删除。
                    </p>
                  </div>
                  <span className="settings-revision">{`修订 ${settings.revision}`}</span>
                </div>
                <div className="settings-server-form">
                  <div className="settings-fields-grid">
                    <NumberField
                      label="正文保留数量"
                      value={settings.log_body_keep_count}
                      min={1}
                      max={100000}
                      hint="保留完整正文的最近日志条数，更早的只留元数据和请求头。"
                      onChange={(v) => patch("log_body_keep_count", v)}
                    />
                    <NumberField
                      label="日志保留天数"
                      value={settings.log_retention_days}
                      min={1}
                      max={3650}
                      hint="超过期限的日志会在下一轮清理时移除。"
                      onChange={(v) => patch("log_retention_days", v)}
                    />
                    <NumberField
                      className="span-2"
                      label="原始正文采集上限（字节）"
                      value={settings.log_body_max_bytes}
                      min={0}
                      max={134217728}
                      hint="最大 128 MiB。设为 0 时只保留元数据和请求头，不采集正文。"
                      onChange={(v) => patch("log_body_max_bytes", v)}
                    />
                    {/* 服务端按 MB 存，这里按 GB 填：图片目录的量级是 GB。显示取两位
                        小数，1229MB 显示成 1.2 而不是 1.2001953125。 */}
                    <NumberField
                      className="span-2"
                      label="生图图片存储上限（GB）"
                      value={Math.round((settings.image_storage_max_mb / 1024) * 100) / 100}
                      min={0}
                      max={1024}
                      step={0.01}
                      hint="生图结果另存为文件，日志里只留链接，链接可直接下载。超出上限时从最旧的图删起，约每五分钟检查一次。设为 0 不再保存，并清空已存的图。"
                      onChange={(v) => patch("image_storage_max_mb", Math.round(v * 1024))}
                    />
                  </div>
                  <div className="settings-save-row">
                    <p className="settings-inline-status" role="status">
                      {`上次更新 ${formatTimestamp(settings.updated_at)}`}
                    </p>
                    <button
                      type="button"
                      className="primary"
                      disabled={savingCard !== null}
                      onClick={() => void save("log")}
                    >
                      {savingCard === "log" ? "保存中…" : "保存日志策略"}
                    </button>
                  </div>
                </div>
              </section>

              <section className="settings-card">
                <div className="settings-card-head">
                  <div>
                    <h3>路由、有效权重与重试</h3>
                    <p>这些参数由服务端保存，并用于保存后的新请求。</p>
                  </div>
                  <span className="settings-readonly-tag">全局</span>
                </div>

                {/* 这四步是路由的全部规则。不写下来的话，下面六个数字看不出彼此的关系。 */}
                <div className="routing-rule-guide">
                  <ol className="routing-rule-steps">
                    <li>
                      <strong>模型优先</strong>
                      <span>先保留模型匹配分最高的渠道；显式指定渠道时跳过池内权重选择。</span>
                    </li>
                    <li>
                      <strong>优先级硬分层</strong>
                      <span>只要最高 priority 组内存在有效权重大于 0 的渠道，就不会使用更低 priority。</span>
                    </li>
                    <li>
                      <strong>同层按权重随机</strong>
                      <span>未固定权重时按有效权重分流；固定权重时始终按基础权重分流。</span>
                    </li>
                    <li>
                      <strong>降到 0 后切换</strong>
                      <span>
                        某一层全部为 0 才回退到下一层。渠道恢复为正数后，高优先级层会立即重新接管流量。
                      </span>
                    </li>
                  </ol>
                  <p className="routing-rule-detail">
                    有效权重由基础权重派生，范围为 0 到基础权重。所有上游非 2xx、连接失败、超时、读取失败和
                    SSE 上游异常都会降低有效权重；正常完成的 2xx 请求会恢复有效权重。有效权重为 0
                    时退出动态池，经过完整恢复周期后重新参与选择。
                  </p>
                  <p className="routing-rule-detail">
                    自动路由的每次重试都会重新选择渠道。选到不同渠道立即重试；再次选到同一渠道才等待。
                    显式指定渠道的重试始终等待。一旦下游响应头已经提交，尤其是成功的 SSE
                    流，后续异常只记失败，不再透明重试。
                  </p>
                </div>

                <div className="settings-server-form">
                  <div className="settings-fields-grid routing-settings-grid">
                    <NumberField
                      label="最大重试次数"
                      value={settings.max_retries}
                      min={0}
                      max={5}
                      hint="额外尝试次数；0 表示不重试。"
                      onChange={(v) => patch("max_retries", v)}
                    />
                    <NumberField
                      label="同渠道重试间隔（毫秒）"
                      value={settings.same_upstream_retry_interval_ms}
                      min={0}
                      max={60000}
                      hint="换到不同渠道时不会等待。"
                      onChange={(v) => patch("same_upstream_retry_interval_ms", v)}
                    />
                    <NumberField
                      label="失败降幅"
                      value={settings.auto_weight_failure_penalty}
                      min={0}
                      max={100}
                      hint="每次渠道失败都会降低动态有效权重。"
                      onChange={(v) => patch("auto_weight_failure_penalty", v)}
                    />
                    <NumberField
                      label="成功恢复幅度"
                      value={settings.auto_weight_success_increment}
                      min={0}
                      max={100}
                      hint="每次正常完成后提高动态有效权重，最高不超过基础权重。"
                      onChange={(v) => patch("auto_weight_success_increment", v)}
                    />
                    <NumberField
                      label="定时恢复幅度"
                      value={settings.auto_weight_recovery_increment}
                      min={0}
                      max={100}
                      hint="有效权重为 0 后，每经过一个完整周期恢复一次。"
                      onChange={(v) => patch("auto_weight_recovery_increment", v)}
                    />
                    <NumberField
                      label="恢复周期（秒）"
                      value={settings.auto_weight_recovery_interval_seconds}
                      min={1}
                      max={3600}
                      hint="有效权重变化后重新开始计时。"
                      onChange={(v) => patch("auto_weight_recovery_interval_seconds", v)}
                    />
                  </div>
                  <div className="settings-save-row">
                    <p className="settings-inline-status" role="status" />
                    <button
                      type="button"
                      className="primary"
                      disabled={savingCard !== null}
                      onClick={() => void save("routing")}
                    >
                      {savingCard === "routing" ? "保存中…" : "保存路由策略"}
                    </button>
                  </div>
                </div>
              </section>

              <section className="settings-card">
                <div className="settings-card-head">
                  <div>
                    <h3>出站代理</h3>
                    <p>
                      启用后网关向上游发起的请求（转发、渠道探测、模型测试）都会经过该代理；保存后对新建立的连接立即生效。
                    </p>
                  </div>
                  <span className="settings-readonly-tag">全局</span>
                </div>
                <div className="settings-server-form">
                  <div className="settings-fields-grid">
                    <div className="toggle-list span-2">
                      <label className="toggle-row">
                        <input
                          type="checkbox"
                          checked={settings.proxy_enabled}
                          onChange={(event) => patch("proxy_enabled", event.target.checked)}
                        />
                        <span>
                          <strong>启用出站代理</strong>
                          <small>
                            关闭时不走此处配置的代理（仍遵循系统 HTTP_PROXY/HTTPS_PROXY 环境变量）。
                          </small>
                        </span>
                      </label>
                    </div>
                    <label className="field span-2">
                      <span className="field-label">代理地址</span>
                      <input
                        type="text"
                        value={settings.proxy_url}
                        onChange={(event) => patch("proxy_url", event.target.value)}
                        placeholder="http://127.0.0.1:7890"
                        spellCheck={false}
                        autoComplete="off"
                      />
                      <span className="field-hint">
                        支持 http://、https://、socks5:// 与 socks5h://，可带账号密码。启用时必填。
                      </span>
                    </label>
                  </div>
                  <div className="settings-save-row">
                    <p className="settings-inline-status" role="status" />
                    <button
                      type="button"
                      className="primary"
                      disabled={savingCard !== null}
                      onClick={() => void save("proxy")}
                    >
                      {savingCard === "proxy" ? "保存中…" : "保存代理设置"}
                    </button>
                  </div>
                </div>
              </section>
            </>
          )}

          <section className="settings-card">
            <div className="settings-card-head">
              <div>
                <h3>模型测试 Prompt</h3>
                <p>测试窗口选择的 Prompt 模板；编辑后下次测试立即生效。</p>
              </div>
              <button
                type="button"
                className="secondary"
                onClick={() => setEditingTemplate({ template: null })}
              >
                新增 Prompt
              </button>
            </div>
            <div className="model-test-template-list" aria-live="polite">
              {templates.length === 0 ? (
                <p className="settings-loading">暂无 Prompt。</p>
              ) : (
                templates.map((template) => (
                  <div key={template.id} className="model-test-template-item">
                    <div>
                      <strong>{template.name}</strong>
                      <p title={template.prompt}>{template.prompt}</p>
                    </div>
                    <div className="model-test-template-actions">
                      <button
                        type="button"
                        className="secondary small"
                        onClick={() => setEditingTemplate({ template })}
                      >
                        编辑
                      </button>
                      <button
                        type="button"
                        className="secondary small danger"
                        onClick={() => void removeTemplate(template)}
                      >
                        删除
                      </button>
                    </div>
                  </div>
                ))
              )}
            </div>
          </section>

          <section className="settings-card">
            <div className="settings-card-head">
              <div>
                <h3>网关默认值</h3>
                <p>渠道没有自己的超时时用这个值；新建渠道也以它为初始值。</p>
              </div>
              <span className="settings-readonly-tag">全局</span>
            </div>
            {settings ? (
              <div className="settings-server-form">
                <div className="settings-fields-grid">
                  <NumberField
                    label="默认上游超时（秒）"
                    value={settings.default_upstream_timeout_seconds}
                    min={0}
                    max={3600}
                    hint={`0 表示沿用启动配置（当前 ${
                      system ? `${system.default_upstream_timeout_seconds} 秒` : "读取中"
                    }）。保存后立即生效，不用重启。`}
                    onChange={(v) => patch("default_upstream_timeout_seconds", v)}
                  />
                </div>
                <div className="settings-save-row">
                  <p className="settings-inline-status" role="status" />
                  <button
                    type="button"
                    className="primary"
                    disabled={savingCard !== null}
                    onClick={() => void save("timeout")}
                  >
                    {savingCard === "timeout" ? "保存中…" : "保存默认超时"}
                  </button>
                </div>
              </div>
            ) : null}
          </section>

          <section className="settings-card">
            <div className="settings-card-head">
              <div>
                <h3>看板显示</h3>
                <p>看板上的请求数和 Tokens 数乘上这个倍率再显示。日志、令牌用量和配额不受影响。</p>
              </div>
              <span className="settings-readonly-tag">全局</span>
            </div>
            {settings ? (
              <div className="settings-server-form">
                <div className="settings-fields-grid">
                  <NumberField
                    label="显示倍率"
                    value={settings.dashboard_multiplier}
                    min={0.01}
                    max={1000}
                    step={0.01}
                    hint="1 表示按实际显示。可填小数，如 1.5；结果取整。"
                    onChange={(v) => patch("dashboard_multiplier", v)}
                  />
                </div>
                <div className="settings-save-row">
                  <p className="settings-inline-status" role="status" />
                  <button
                    type="button"
                    className="primary"
                    disabled={savingCard !== null}
                    onClick={() => void save("dashboard")}
                  >
                    {savingCard === "dashboard" ? "保存中…" : "保存显示倍率"}
                  </button>
                </div>
              </div>
            ) : null}
          </section>

          <section className="settings-card settings-security">
            <div className="settings-card-head">
              <div>
                <h3>安全</h3>
                <p>管理员令牌可由你自行设置；更换后旧令牌会立即失效。</p>
              </div>
              <span className="settings-security-mark">敏感操作</span>
            </div>
            <div className="settings-action-row">
              <div>
                <strong>更换管理员令牌</strong>
                <p>保存后当前控制台会自动改用新令牌，新值只显示一次。</p>
              </div>
              <button type="button" className="danger" onClick={() => setRotating(true)}>
                更换令牌
              </button>
            </div>
            {rotatedToken ? (
              <label className="field">
                <span className="field-label">新令牌</span>
                <input readOnly value={rotatedToken} />
                <span className="field-hint">这个值不会再显示，现在就存好。</span>
              </label>
            ) : null}
          </section>

          <section className="settings-card">
            <div className="settings-card-head">
              <div>
                <h3>运行信息</h3>
                <p>只展示服务状态与汇总信息，不包含环境路径或秘密。</p>
              </div>
              <button
                type="button"
                className="secondary"
                disabled={refreshingSystem}
                onClick={() => void refreshSystem()}
              >
                {refreshingSystem ? "刷新中…" : "刷新"}
              </button>
            </div>
            <div className="system-info-grid" aria-live="polite">
              {!system || !metrics || !cleanup ? (
                <p className="settings-loading">加载运行信息…</p>
              ) : (
                <>
                  <InfoItem label="服务" value={system.service || "WildToken"} />
                  <InfoItem label="版本" value={system.version || "—"} />
                  <InfoItem label="运行时长" value={formatUptime(system.uptime_seconds)} />
                  <InfoItem label="当前服务器时间" value={system.current_server_time || "—"} />
                  <InfoItem label="数据库" value={system.database_ok ? "连接正常" : "不可用"} />
                  <InfoItem label="数据库已分配" value={formatBytes(system.database_allocated_bytes)} />
                  <InfoItem label="日志总数" value={formatCount(system.total_log_count)} />
                  <InfoItem label="近 24 小时日志" value={formatCount(system.log_count_24h)} />
                  <InfoItem
                    label="启用渠道"
                    value={`${system.enabled_upstream_count} / ${system.total_upstream_count}`}
                  />
                  <InfoItem
                    label="近 1 分钟成功请求"
                    value={formatCount(system.recent_one_minute_log_count)}
                  />
                  <InfoItem label="活跃 SSE" value={formatCount(metrics.active_sse_streams)} />
                  <InfoItem
                    label="10 分钟 SSE 断连"
                    value={formatCount(metrics.sse_recent_disconnects_10m)}
                  />
                  <InfoItem
                    label="SSE 断连总数"
                    value={formatCount(metrics.sse_client_disconnects_total)}
                  />
                  <InfoItem label="SSE 上游错误" value={formatCount(metrics.sse_upstream_errors_total)} />
                  <InfoItem label="日志队列" value={formatCount(metrics.log_queue_depth)} />
                  <InfoItem label="日志写入" value={formatCount(metrics.log_written_total)} />
                  <InfoItem label="日志写批次" value={formatCount(metrics.log_write_batches_total)} />
                  <InfoItem label="日志丢弃" value={formatCount(metrics.log_dropped_total)} />
                  <InfoItem label="日志写失败" value={formatCount(metrics.log_write_failures_total)} />
                  <InfoItem label="慢 DB 操作" value={formatCount(metrics.slow_db_operations_total)} />
                  <InfoItem label="清理任务" value={cleanup.active ? "运行中" : "空闲"} />
                  <InfoItem
                    label="清理进度"
                    value={
                      cleanup.active
                        ? `${formatCount(cleanup.current_rows_cleared)} 行 / ${formatCount(cleanup.current_batches)} 批`
                        : `${formatCount(cleanup.last_rows_cleared ?? 0)} 行 · ${formatDuration(cleanup.last_duration_ms)}`
                    }
                  />
                </>
              )}
            </div>
          </section>
        </div>
      </section>

      <PromptDialog
        open={editingTemplate !== null}
        template={editingTemplate?.template ?? null}
        onSubmit={(name, prompt) => void saveTemplate(name, prompt)}
        onClose={() => setEditingTemplate(null)}
      />
      <RotateDialog
        open={rotating}
        onSubmit={(token) => void rotate(token)}
        onClose={() => setRotating(false)}
      />
    </section>
  );
}

function InfoItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="system-info-item">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function NumberField({
  label,
  value,
  min,
  max,
  step,
  hint,
  className,
  onChange,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  /** 不给时浏览器按 1 校验，小数会被判非法。 */
  step?: number;
  hint?: string;
  className?: string;
  onChange: (value: number) => void;
}) {
  /* 输入时以敲的原文为准。受控值可能是换算出来的（GB 由 MB 反推），每敲一
     位就写回的话，1.2 会被改写成 1.2001953125，小数根本输不进去。 */
  const [draft, setDraft] = useState<string | null>(null);

  return (
    <label className={className ? `field ${className}` : "field"}>
      <span className="field-label">{label}</span>
      <input
        type="number"
        min={min}
        max={max}
        step={step}
        required
        inputMode={step !== undefined && step < 1 ? "decimal" : "numeric"}
        value={draft ?? value}
        onFocus={() => setDraft(String(value))}
        onBlur={() => setDraft(null)}
        onChange={(event) => {
          setDraft(event.target.value);
          /* 清空不回写：原先按 0 存，图片上限清空就是删光已存的图，重试次数
             清空就是不重试。失焦后草稿作废，框里回到原值。 */
          const raw = event.target.value.trim();
          const value = Number(raw);
          if (raw !== "" && Number.isFinite(value)) onChange(value);
        }}
      />
      {hint ? <span className="field-hint">{hint}</span> : null}
    </label>
  );
}

/** 管理员令牌的形状，和后端校验一致：8–256 位可打印 ASCII，不含空格。 */
const ADMIN_TOKEN_PATTERN = /^[\x21-\x7E]{8,256}$/;

/** 32 位字母数字。丢掉 248 以上的字节，每个字符才等概率。 */
function randomAdminToken(): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let token = "";
  while (token.length < 32) {
    for (const byte of crypto.getRandomValues(new Uint8Array(32))) {
      if (byte < 248 && token.length < 32) token += alphabet[byte % alphabet.length];
    }
  }
  return token;
}

/**
 * 更换管理员令牌。新值由管理员填或随机生成，和旧控制台一致：后端不代为
 * 生成，也不在响应里回令牌。
 */
function RotateDialog({
  open,
  onSubmit,
  onClose,
}: {
  open: boolean;
  onSubmit: (token: string) => void;
  onClose: () => void;
}) {
  const ref = useDialog(open, onClose);
  const [token, setToken] = useState("");
  const [understood, setUnderstood] = useState(false);

  useEffect(() => {
    if (!open) return;
    setToken("");
    setUnderstood(false);
  }, [open]);

  const valid = ADMIN_TOKEN_PATTERN.test(token.trim());

  return (
    <dialog className="confirm-dialog" ref={ref} onCancel={onClose} aria-label="更换管理员令牌">
      <form
        className="confirm-panel"
        onSubmit={(event) => {
          event.preventDefault();
          if (valid && understood) onSubmit(token.trim());
        }}
      >
        <div className="modal-head">
          <div>
            <h2>更换管理员令牌</h2>
            <p>保存后旧令牌立即失效，其他已登录的浏览器会被登出，当前控制台自动改用新令牌。</p>
          </div>
        </div>

        <div className="form-grid">
          <label className="field span-2">
            <span className="field-label">新令牌</span>
            <input
              value={token}
              onChange={(event) => setToken(event.target.value)}
              minLength={8}
              maxLength={256}
              required
              spellCheck={false}
              autoComplete="new-password"
              placeholder="8–256 位可打印字符，不含空格"
            />
            <span className="field-hint">保存前先把它存好，之后只会在本页显示一次。</span>
          </label>
          <label className="rotate-confirm-check span-2">
            <input
              type="checkbox"
              checked={understood}
              onChange={(event) => setUnderstood(event.target.checked)}
            />
            <span>我了解保存后旧令牌会立即失效。</span>
          </label>
        </div>

        <div className="modal-actions">
          <button type="button" className="secondary" onClick={() => setToken(randomAdminToken())}>
            随机生成
          </button>
          <button type="button" className="secondary" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="danger" disabled={!valid || !understood}>
            保存并更换
          </button>
        </div>
      </form>
    </dialog>
  );
}

function PromptDialog({
  open,
  template,
  onSubmit,
  onClose,
}: {
  open: boolean;
  template: PromptTemplate | null;
  onSubmit: (name: string, prompt: string) => void;
  onClose: () => void;
}) {
  const ref = useDialog(open, onClose);
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");

  useEffect(() => {
    if (!open) return;
    setName(template?.name ?? "");
    setPrompt(template?.prompt ?? "");
  }, [open, template]);

  return (
    <dialog className="confirm-dialog" ref={ref} onCancel={onClose} aria-label="Prompt 模板">
      <form
        className="confirm-panel"
        onSubmit={(event) => {
          event.preventDefault();
          if (!name.trim() || !prompt.trim()) return;
          onSubmit(name.trim(), prompt.trim());
        }}
      >
        <div className="modal-head">
          <div>
            <h2>{template ? `编辑 Prompt #${template.id}` : "新增 Prompt"}</h2>
            <p>测试窗口会从这些模板中选择。</p>
          </div>
          <div className="modal-head-actions">
            <button
              type="button"
              className="secondary ghost icon-close"
              aria-label="关闭"
              title="关闭"
              onClick={onClose}
            >
              <svg className="dialog-icon dialog-icon--close" viewBox="0 0 16 16" aria-hidden="true">
                <path d="M4 4l8 8M12 4L4 12" />
              </svg>
            </button>
          </div>
        </div>

        <div className="form-grid">
          <label className="field span-2">
            <span className="field-label">Prompt 名称</span>
            <input
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={80}
              required
              placeholder="例如：代码审查"
              autoComplete="off"
            />
          </label>
          <label className="field span-2">
            <span className="field-label">Prompt 内容</span>
            <textarea
              rows={8}
              maxLength={20000}
              required
              value={prompt}
              onChange={(event) => setPrompt(event.target.value)}
            />
          </label>
        </div>

        <div className="modal-actions">
          <button type="button" className="secondary" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="primary" disabled={!name.trim() || !prompt.trim()}>
            保存 Prompt
          </button>
        </div>
      </form>
    </dialog>
  );
}
