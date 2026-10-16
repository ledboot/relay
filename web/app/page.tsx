"use client";
import { useState, useEffect, useRef } from "react";
import {
  ArrowRight,
  ShieldCheck,
  Copy,
  Check,
  MessageSquare,
  KeyRound,
  Smartphone,
  LoaderCircle,
  RefreshCw,
} from "lucide-react";
type Message = { code: string; content: string; at: string };
type Order = {
  service: string;
  state: string;
  phone: string;
  code: string;
  message: string;
  endsAt: string;
  reservationType: string;
  duration: string;
  messages?: Message[];
};
const labels: Record<string, string> = {
  ready: "待兑换",
  allocating: "正在申请号码",
  waiting: "等待短信",
  used: "已收到短信",
  expired: "订单已结束",
  timed_out: "接码超时（未收到验证码）",
  canceled: "已取消",
  refunded: "已退款",
  reported: "已举报",
  disabled: "已禁用",
  rental_active: "租用中",
  rental_expired: "租用已到期",
  rental_refunded: "已退款",
};
// 租用号码能活好几天，但页面不该无限轮询：一次连续接收最多 10 分钟，可手动继续。
const RENTAL_SESSION_SECONDS = 600;
// 接码到期后再多轮询两分钟，等上游把超时状态同步回来。
const VERIFICATION_GRACE_SECONDS = 120;
function formatClock(seconds: number): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}
// 租用短信可能跨天，列表时间需要带日期。固定 24 小时制：toLocaleTimeString() 跟随浏览器
// locale，en-US 会输出 “4:29:22 PM”。
function formatStamp(value: string): string {
  const t = new Date(value);
  if (Number.isNaN(t.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}:${pad(t.getSeconds())}`;
}
// 租用有效期以天计，只显示时分会让“有效期至”产生歧义。
function formatDateTime(value: string): string {
  const t = new Date(value);
  if (Number.isNaN(t.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
}
export default function Home() {
  const [key, setKey] = useState("");
  const [active, setActive] = useState("");
  const [order, setOrder] = useState<Order | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState("");
  // watching 表示页面正在自动接收；expiry 是展示用的到期时间（接码），until 是本页
  // 停止轮询的时间。tick 每秒推进一次，倒计时不依赖 setInterval 的准确度。
  const [watching, setWatching] = useState(false);
  const [expiry, setExpiry] = useState(0);
  const [until, setUntil] = useState(0);
  const [tick, setTick] = useState(0);
  // 浏览器时钟可能不准，用响应头的 Date 校正，否则倒计时会虚高或变成负数。
  const skew = useRef(0);
  // 租用订单的有效期以天计，需要展示完整日期，且收到短信也不结束。
  const rental = order ? order.reservationType !== "verification" : false;
  const remaining = (target: number) =>
    target && tick ? Math.max(0, Math.ceil((target - tick) / 1000)) : 0;
  const serverNow = () => Date.now() + skew.current;
  // 开启一次自动接收窗口：接码跟着号码有效期（再加一点宽限），租用用固定会话时长。
  function watchFor(d: Order) {
    const now = serverNow();
    const cap = now + 15 * 60 * 1000;
    let expires = 0;
    if (d.reservationType === "verification") {
      const end = Date.parse(d.endsAt);
      expires = Number.isNaN(end) ? cap : Math.min(Math.max(end, now), cap);
    }
    setExpiry(expires);
    setUntil(expires ? expires + VERIFICATION_GRACE_SECONDS * 1000 : now + RENTAL_SESSION_SECONDS * 1000);
    setTick(now);
    setWatching(true);
  }
  async function request(endpoint: string, value: string) {
    const r = await fetch("/api/" + endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key: value }),
    });
    const date = r.headers.get("date");
    if (date) {
      const server = Date.parse(date);
      if (!Number.isNaN(server)) skew.current = server - Date.now();
    }
    const d = await r.json();
    if (!r.ok) throw new Error(d.error);
    return d as Order;
  }
  async function redeem() {
    if (!key.trim()) return;
    setBusy(true);
    setError("");
    setOrder(null);
    setActive("");
    setWatching(false);
    try {
      const d = await request("redeem", key);
      setOrder(d);
      setActive(key);
      watchFor(d);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  // 继续接收：先取一次最新状态（暂停期间的短信也在服务端），再开一个新窗口。
  async function resume() {
    if (!active || busy) return;
    setBusy(true);
    setError("");
    try {
      const d = await request("status", active);
      setOrder(d);
      watchFor(d);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  // 退出只结束本页的接收；租用号码和已收短信都保留在服务端，重新输入密钥即可查看。
  function exit() {
    setWatching(false);
    setOrder(null);
    setActive("");
    setKey("");
    setError("");
  }
  useEffect(() => {
    if (!active || !watching) return;
    if (order?.state !== "waiting" && order?.state !== "rental_active") return;
    let live = true;
    const t = setInterval(async () => {
      try {
        const d = await request("status", active);
        if (live) {
          setOrder(d);
          setError("");
        }
      } catch (e) {
        if (live) setError((e as Error).message);
      }
    }, order?.state === "rental_active" ? 10000 : 6000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [active, watching, order?.state]);
  useEffect(() => {
    if (!watching) return;
    setTick(serverNow());
    const t = setInterval(() => setTick(serverNow()), 1000);
    return () => clearInterval(t);
  }, [watching]);
  useEffect(() => {
    if (watching && until && tick >= until) setWatching(false);
  }, [watching, until, tick]);
  async function retry() {
    // 用已兑换的密钥重试，不受输入框后续修改影响。
    if (!active || busy) return;
    setBusy(true);
    setError("");
    try {
      setOrder(await request("redeem", active));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(value);
      setTimeout(() => setCopied(""), 1800);
    } catch {
      setError("复制失败，请手动复制");
    }
  }
  return (
    <>
      <header className="header customer-header">
        <a className="brand" href="/">
          <img className="brand-icon" src="/branding/logo.png" alt="" width={40} height={40} />
          relay<span className="brand-dot">.</span>
        </a>
        <span className="brand-slogan">短信验证服务</span>
      </header>
      <main className="customer">
        <section className="hero">
          <div className="eyebrow">
            <span /> SIMPLIFY YOUR VERIFICATION
          </div>
          <h1>
            一枚密钥，
            <br />
            连接你的<span>数字世界。</span>
          </h1>
          <p>
            兑换专属号码，接收短信验证码。
            <br />
            支持一次性验证与长期租用，让每一步验证更轻松。
          </p>
          <div className="platforms">
            <span>
              <img src="/platforms/openai.svg" alt="" width={18} height={18} />
              OpenAI
            </span>
            <span>
              <img src="/platforms/telegram.svg" alt="" width={18} height={18} />
              Telegram
            </span>
            <span>
              <img src="/platforms/whatsapp.svg" alt="" width={18} height={18} />
              WhatsApp
            </span>
            <span className="muted">及更多平台</span>
          </div>
          <div className="hero-note">
            <ShieldCheck size={17} /> 密钥专属兑换 · 按次接码与长期租用
          </div>
        </section>
        <section className="redeem-card">
          <div className="card-top">
            <div className="icon-box">
              <KeyRound size={22} />
            </div>
            <span className="small-tag">CD KEY 兑换</span>
          </div>
          <h2>开始你的验证</h2>
          <p className="muted">输入兑换密钥，获取对应平台的专属号码。</p>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              redeem();
            }}
          >
            <label htmlFor="key">兑换密钥</label>
            <input
              id="key"
              autoComplete="off"
              spellCheck={false}
              placeholder="RELAY-OPENAI-XXXXXXXXXXXXXXXX"
              value={key}
              onChange={(e) => setKey(e.target.value)}
              required
              maxLength={100}
            />
            <button className="primary" disabled={busy || !key.trim()}>
              {busy ? (
                <LoaderCircle className="spin" size={18} />
              ) : (
                <KeyRound size={17} />
              )}{" "}
              {busy ? "正在申请号码…" : "兑换并获取号码"}{" "}
              {!busy && <ArrowRight size={18} />}
            </button>
          </form>
          <div className="secure">
            <ShieldCheck size={14} /> 已兑换的密钥可再次输入，查看接收结果
          </div>
          {error && (
            <div className="error" role="alert">
              {error}
            </div>
          )}
          {order && (
            <div className="result" aria-live="polite">
              <div className="result-head">
                <strong>{order.service}</strong>
                <span className={"badge " + order.state}>
                  {labels[order.state] || order.state}
                </span>
              </div>
              {order.phone && (
                <div className="value-row">
                  <div>
                    <small>接收号码</small>
                    <strong>{order.phone}</strong>
                  </div>
                  <button
                    className="copy"
                    aria-label="复制号码"
                    onClick={() => copy(order.phone)}
                  >
                    {copied === order.phone ? (
                      <Check size={18} />
                    ) : (
                      <Copy size={18} />
                    )}
                  </button>
                </div>
              )}
              {order.code && (
                <div className="value-row">
                  <div>
                    <small>验证码</small>
                    <strong className="otp">{order.code}</strong>
                  </div>
                  <button
                    className="copy"
                    aria-label="复制验证码"
                    onClick={() => copy(order.code)}
                  >
                    <Copy size={18} />
                  </button>
                </div>
              )}
              {order.message && <p className="sms">{order.message}</p>}
              {rental && (order.state === "rental_active" || order.state === "rental_expired") && (
                <div className="messages">
                  <div className="messages-head">
                    <small>
                      收到的短信
                      {order.messages && order.messages.length > 0
                        ? ` · ${order.messages.length} 条`
                        : ""}
                    </small>
                  </div>
                  {order.messages && order.messages.length > 0 ? (
                    order.messages.map((m, i) => (
                      <div className="message" key={`${m.at}-${m.code}-${i}`}>
                        <div className="message-head">
                          <small>{formatStamp(m.at)}</small>
                          {m.code && (
                            <button
                              className="copy"
                              aria-label="复制验证码"
                              onClick={() => copy(m.code)}
                            >
                              {copied === m.code ? <Check size={16} /> : <Copy size={16} />}
                            </button>
                          )}
                        </div>
                        {m.code && <strong className="otp">{m.code}</strong>}
                        {m.content && <p className="sms">{m.content}</p>}
                      </div>
                    ))
                  ) : (
                    <p className="muted">
                      尚未收到短信。新短信会自动出现在最上面。
                    </p>
                  )}
                </div>
              )}
              {order.state === "rental_active" && (
                <p className="waiting">
                  <span className="pulse" /> 租用中，号码在有效期内一直可用
                  {order.endsAt && formatDateTime(order.endsAt) && (
                    <small className="expiry">
                      有效期至 {formatDateTime(order.endsAt)}
                    </small>
                  )}
                </p>
              )}
              {order.state === "rental_expired" && (
                <p>租用期已结束。如需继续使用，请重新购买一枚密钥。</p>
              )}
              {order.state === "rental_refunded" && (
                <p>上游订单已退款，请联系管理员处理卡密。</p>
              )}
              {order.state === "waiting" && (
                <p className="waiting">
                  <span className="pulse" /> 自动接收中，无需刷新
                  {expiry > 0 && (
                    <small className="expiry">
                      {remaining(expiry) > 0
                        ? `号码有效期剩余 ${formatClock(remaining(expiry))}`
                        : "号码已到期，正在等待上游确认…"}
                    </small>
                  )}
                </p>
              )}
              {rental && order.state === "rental_active" && watching && (
                <p className="waiting">
                  <small className="expiry">
                    本页自动接收剩余 {formatClock(remaining(until))}
                  </small>
                </p>
              )}
              {!watching && (order.state === "waiting" || order.state === "rental_active") && (
                <div className="retry">
                  <p className="muted">
                    已暂停自动接收，页面不再请求上游。暂停期间收到的短信仍在号码上，继续后即可看到。
                  </p>
                  <div className="actions">
                    <button className="primary" onClick={resume} disabled={busy}>
                      {busy ? (
                        <LoaderCircle className="spin" size={18} />
                      ) : (
                        <RefreshCw size={17} />
                      )}{" "}
                      {busy ? "正在查询…" : "继续接收"}
                    </button>
                    <button className="secondary" onClick={exit} disabled={busy}>
                      退出
                    </button>
                  </div>
                </div>
              )}
              {watching && (order.state === "waiting" || order.state === "rental_active") && (
                <div className="actions">
                  <button className="secondary" onClick={exit} disabled={busy}>
                    退出本次接收
                  </button>
                </div>
              )}
              {order.state === "timed_out" && (
                <div className="retry">
                  <p className="muted">本次号码已超时，未收到验证码。</p>
                  <p>你的 CD Key 仍然有效，可以直接重新兑换一个新号码。</p>
                  <button className="primary" onClick={retry} disabled={busy}>
                    {busy ? (
                      <LoaderCircle className="spin" size={18} />
                    ) : (
                      <RefreshCw size={17} />
                    )}{" "}
                    {busy ? "正在重新申请号码…" : "重新兑换"}
                  </button>
                </div>
              )}
              {order.state === "refunded" && <p>上游订单已退款，请联系管理员处理卡密。</p>}
              {order.state === "canceled" && <p>号码申请已取消，请联系管理员处理。</p>}
              {order.state === "reported" && <p>该号码已举报，请联系管理员处理。</p>}
              {order.state === "expired" && (
                <p>本次号码已结束且未收到短信，请联系管理员处理。</p>
              )}
            </div>
          )}
        </section>
        <section id="guide" className="guide">
          <div className="section-label">
            HOW IT WORKS <span>三步完成验证</span>
          </div>
          <div className="steps">
            {[
              {
                icon: KeyRound,
                title: "输入兑换密钥",
                text: "使用你获得的 CD Key，兑换对应平台的号码。",
              },
              {
                icon: Smartphone,
                title: "使用专属号码",
                text: "将号码填写到目标平台，申请发送验证码。",
              },
              {
                icon: MessageSquare,
                title: "获取短信验证码",
                text: rental
                  ? "租用订单可直接查看号码与有效期，有效期内一直可用。"
                  : "保持页面打开，短信到达后会自动显示。",
              },
            ].map((s, i) => (
              <article key={s.title}>
                <span className="step-number">0{i + 1}</span>
                <s.icon size={23} />
                <h3>{s.title}</h3>
                <p>{s.text}</p>
              </article>
            ))}
          </div>
        </section>
      </main>
      <footer>
        <span>© {new Date().getFullYear()} Relay. 短信验证服务</span>
      </footer>
    </>
  );
}
