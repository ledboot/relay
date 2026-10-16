"use client";
import { useState, useEffect } from "react";
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
type Order = {
  service: string;
  state: string;
  phone: string;
  code: string;
  message: string;
  endsAt: string;
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
};
// 固定 24 小时制。toLocaleTimeString() 跟随浏览器 locale，en-US 会输出 “4:29:22 PM”。
function formatTime24(value: string): string {
  const t = new Date(value);
  if (Number.isNaN(t.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(t.getHours())}:${pad(t.getMinutes())}:${pad(t.getSeconds())}`;
}
export default function Home() {
  const [key, setKey] = useState("");
  const [active, setActive] = useState("");
  const [order, setOrder] = useState<Order | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState("");
  async function request(endpoint: string, value: string) {
    const r = await fetch("/api/" + endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key: value }),
    });
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
    try {
      setOrder(await request("redeem", key));
      setActive(key);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  useEffect(() => {
    if (!active || order?.state !== "waiting") return;
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
    }, 6000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [active, order?.state]);
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
            简单、直接，让每一步验证更轻松。
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
            <ShieldCheck size={17} /> 密钥专属兑换 · 收到短信后自动核销
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
              {order.state === "waiting" && (
                <p className="waiting">
                  <span className="pulse" /> 自动接收中，无需刷新
                  {order.endsAt && formatTime24(order.endsAt) && (
                    <small className="expiry">
                      有效期至 {formatTime24(order.endsAt)}
                    </small>
                  )}
                </p>
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
                text: "保持页面打开，短信到达后会自动显示。",
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
