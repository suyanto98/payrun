// PR-3 (docs/design/26-performance-plan.md §5): 100.000 karyawan, API baca dengan RLS. Closed model,
// VU tersebar di tenant dengan bobot ukuran (tenants.json dari run.sh). Setiap item karyawan divalidasi:
// departemennya harus milik tenant VU itu (penjaga kebocoran lintas tenant; target 0).
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8091/api/v1';
const PASSWORD = 'correct horse battery'; // app.SeedPassword
const tenants = JSON.parse(open(__ENV.TENANTS_FILE));
const leaks = new Counter('cross_tenant_rows');
const VUS = Number(__ENV.VUS || 300);

// RATE (iterasi/dtk, ±8 request per iterasi) → open model constant-arrival-rate: latency di bawah saturasi
// (S9: closed model tanpa think time hanya mengukur antrean, p95 ≈ VU / throughput). Tanpa RATE: closed 300 VU.
const RATE = Number(__ENV.RATE || 0);
export const options = {
  scenarios: RATE > 0
    ? { read: { executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s', duration: __ENV.DURATION || '10m',
        preAllocatedVUs: VUS, maxVUs: VUS } }
    : { read: { executor: 'constant-vus', vus: VUS, duration: __ENV.DURATION || '10m' } },
  setupTimeout: '10m',
  thresholds: {
    'http_req_duration{name:employees_page}': ['p(95)<300'],
    'http_req_duration{name:employees_filter}': ['p(95)<300'],
    'http_req_duration{name:employees_prefix}': ['p(95)<300'],
    'http_req_duration{name:attendance_day}': ['p(95)<300'],
    cross_tenant_rows: ['count==0'],
    checks: ['rate>0.999'],
  },
};

// setup: satu tenant per VU, dipilih berbobot ukuran (tenant besar lebih sering), login OWNER-nya.
export function setup() {
  const total = tenants.reduce((s, t) => s + t.size, 0);
  const picks = [];
  for (let v = 0; v < VUS; v++) {
    let r = ((v * 7919) % total) + 0.5; // deterministik, tersebar
    let t = tenants[0];
    for (const x of tenants) { r -= x.size; if (r <= 0) { t = x; break; } }
    picks.push(t);
  }
  const out = [];
  for (let i = 0; i < picks.length; i += 6) {
    const batch = picks.slice(i, i + 6);
    const res = http.batch(batch.map((t) => ({ method: 'POST', url: `${BASE}/auth/login`,
      body: JSON.stringify({ email: t.owner, password: PASSWORD }), params: { headers: { 'Content-Type': 'application/json' } } })));
    res.forEach((r, k) => {
      check(r, { 'login 200': (x) => x.status === 200 }) || console.error(`login ${batch[k].owner}: ${r.status}`);
      out.push({ token: r.json('access_token'), depts: batch[k].depts });
    });
  }
  return out;
}

function list(s, url, name) {
  const r = http.get(url, { headers: { Authorization: `Bearer ${s.token}` }, tags: { name } });
  check(r, { [`${name} 200`]: (x) => x.status === 200 });
  if (r.status !== 200) return null;
  const body = r.json();
  return body;
}

function guardEmployees(s, body) {
  for (const it of body.data) {
    if (!it.department || s.depts.indexOf(it.department.id) < 0) leaks.add(1);
  }
}

export default function (data) {
  const s = data[(__VU - 1) % data.length];
  // 5 halaman cursor.
  let cursor = '';
  for (let p = 0; p < 5; p++) {
    const b = list(s, `${BASE}/employees?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, 'employees_page');
    if (!b) break;
    guardEmployees(s, b);
    if (!b.page.has_more) break;
    cursor = b.page.next_cursor;
  }
  const dept = s.depts[Math.floor(Math.random() * s.depts.length)];
  const f = list(s, `${BASE}/employees?status=ACTIVE&department_id=${dept}&limit=50`, 'employees_filter');
  if (f) guardEmployees(s, f);
  const letter = String.fromCharCode(65 + Math.floor(Math.random() * 26));
  const q = list(s, `${BASE}/employees?q=${letter}a&limit=50`, 'employees_prefix');
  if (q) guardEmployees(s, q);
  const day = ['2026-09-01', '2026-09-02', '2026-09-03'][Math.floor(Math.random() * 3)];
  list(s, `${BASE}/attendance?date=${day}&limit=200`, 'attendance_day');
}
