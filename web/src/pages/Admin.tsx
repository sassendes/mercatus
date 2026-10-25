import { useState, type FormEvent } from "react";
import { api, errorMessage } from "../api";
import { Alert, Loading } from "../components/Feedback";
import { Page } from "../components/Layout";
import { formatDate, formatMoney } from "../format";
import { useFetch, useTitle } from "../hooks";

interface Stats {
  users: number;
  active_sellers: number;
  suspended_sellers: number;
  active_products: number;
  removed_products: number;
  paid_orders: number;
  gmv_cents: number;
  commission_cents: number;
}

interface Action {
  id: number;
  admin_id: number;
  target_type: string;
  target_id: number;
  action: string;
  reason: string;
  created_at: string;
}

const actions: Record<string, string[]> = {
  product: ["remove", "reinstate"],
  seller: ["suspend", "reinstate"],
  review: ["remove"],
  buyer: ["block", "unblock"],
};

export default function Admin() {
  useTitle("Admin console");
  const { data: stats, reload: reloadStats } = useFetch<Stats>("/admin/stats");
  const { data: audit, reload } = useFetch<Action[]>("/admin/audit");
  const { data: ledger } = useFetch<{ balanced: boolean; ledger_sum_cents: number }>("/payments/ledger/integrity");
  const [target, setTarget] = useState("product");
  const [action, setAction] = useState("remove");
  const [id, setId] = useState("");
  const [reason, setReason] = useState("");
  const [msg, setMsg] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setMsg(null);
    try {
      await api("/admin/moderate", { method: "POST", json: { target_type: target, target_id: Number(id), action, reason } });
      setMsg({ tone: "ok", text: `Applied ${action} to ${target} ${id}.` });
      setId("");
      setReason("");
      reload();
      reloadStats();
    } catch (err) {
      setMsg({ tone: "bad", text: errorMessage(err) });
    }
  }

  const tiles: [string, string][] = stats
    ? [
        ["Users", String(stats.users)],
        ["Live sellers", String(stats.active_sellers)],
        ["Live products", String(stats.active_products)],
        ["Paid orders", String(stats.paid_orders)],
        ["Gross sales", formatMoney(stats.gmv_cents)],
        ["Commission earned", formatMoney(stats.commission_cents)],
      ]
    : [];

  return (
    <Page narrow>
      <h1 className="text-[26px] font-normal">Admin console</h1>
      {!stats ? (
        <Loading />
      ) : (
        <dl className="panel mt-4 grid grid-cols-2 divide-line sm:grid-cols-3">
          {tiles.map(([k, v]) => (
            <div key={k} className="border-b border-r border-line p-4">
              <dt className="text-xs text-muted">{k}</dt>
              <dd className="text-[19px] font-bold tnum">{v}</dd>
            </div>
          ))}
        </dl>
      )}
      {ledger && (
        <div className="mt-4">
          <Alert tone={ledger.balanced ? "ok" : "bad"}>
            {ledger.balanced ? "Ledger is balanced. Every payment and refund nets to zero." : `Ledger is off by ${formatMoney(ledger.ledger_sum_cents)}. Investigate before paying out.`}
          </Alert>
        </div>
      )}
      <form onSubmit={submit} className="panel mt-5 grid gap-4 p-5 sm:grid-cols-[140px_140px_120px_1fr_auto] sm:items-end">
        <div>
          <label htmlFor="tt" className="label">Target</label>
          <select id="tt" className="field" value={target} onChange={(e) => { setTarget(e.target.value); setAction(actions[e.target.value][0]); }}>
            {Object.keys(actions).map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div>
          <label htmlFor="ta" className="label">Action</label>
          <select id="ta" className="field" value={action} onChange={(e) => setAction(e.target.value)}>
            {actions[target].map((a) => <option key={a} value={a}>{a}</option>)}
          </select>
        </div>
        <div>
          <label htmlFor="tid" className="label">ID</label>
          <input id="tid" className="field tnum" inputMode="numeric" value={id} onChange={(e) => setId(e.target.value)} />
        </div>
        <div>
          <label htmlFor="tr" className="label">Reason</label>
          <input id="tr" className="field" value={reason} onChange={(e) => setReason(e.target.value)} maxLength={500} />
        </div>
        <button className="btn-primary" disabled={!id || reason.length < 3}>Apply</button>
      </form>
      {msg && <div className="mt-3"><Alert tone={msg.tone}>{msg.text}</Alert></div>}
      <section className="panel mt-5">
        <h2 className="border-b border-line px-5 py-3 text-[17px] font-bold">Audit log</h2>
        {audit && audit.length === 0 && <p className="p-5 text-sm text-muted">No moderation actions yet.</p>}
        <ul className="divide-y divide-line text-sm">
          {audit?.map((a) => (
            <li key={a.id} className="flex flex-wrap justify-between gap-2 px-5 py-3">
              <span><span className="font-semibold capitalize">{a.action}</span> {a.target_type} {a.target_id}: {a.reason}</span>
              <time className="text-xs text-muted">{formatDate(a.created_at)}</time>
            </li>
          ))}
        </ul>
      </section>
    </Page>
  );
}
