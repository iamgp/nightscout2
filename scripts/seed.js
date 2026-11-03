// Development helper: seeds a Tideline server with 14 days of synthetic but
// plausible data, posting through the Nightscout API like a real uploader.
// Never point this at a server holding real data.
//
//   mise run seed            (uses TIDELINE_URL and TIDELINE_API_SECRET)
const crypto = require('crypto');
const BASE = process.env.TIDELINE_URL || 'http://localhost:8080';
const SECRET = process.env.TIDELINE_API_SECRET || 'dev-secret-change-me';
const sha = crypto.createHash('sha1').update(SECRET).digest('hex');
const H = { 'Content-Type': 'application/json', 'api-secret': sha };

let seed = 7;
const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647);

const MIN = 60000;
const now = Date.now() - (Date.now() % (5 * MIN));
const start = now - 14 * 24 * 60 * MIN;
const tzOff = new Date().getTimezoneOffset() * MIN;

function meals(dayStart) {
  return [
    { at: dayStart + (7.5 + rnd()) * 60 * MIN, carbs: 40 + Math.round(rnd() * 20) },
    { at: dayStart + (12.5 + rnd()) * 60 * MIN, carbs: 50 + Math.round(rnd() * 30) },
    { at: dayStart + (18.5 + rnd()) * 60 * MIN, carbs: 60 + Math.round(rnd() * 30) },
  ];
}

const entries = [], treatments = [];
const events = [];
for (let d = start - (start % (24 * 60 * MIN)) + tzOff; d < now + 24 * 60 * MIN; d += 24 * 60 * MIN) {
  for (const m of meals(d)) if (m.at < now - 30 * MIN) events.push(m);
}
let v = 120, drift = 0;
for (let t = start; t <= now; t += 5 * MIN) {
  let target = 115 + 15 * Math.sin((t / (60 * MIN)) / 24 * 2 * Math.PI - 1.2);
  for (const e of events) {
    const dt = (t - e.at) / MIN;
    if (dt > 0 && dt < 240) target += e.carbs * 2.2 * Math.exp(-((dt - 60) ** 2) / 1800) - (dt > 120 ? e.carbs * 0.6 * Math.exp(-((dt - 170) ** 2) / 1500) : 0);
  }
  drift += (rnd() - 0.5) * 3;
  drift *= 0.95;
  v += (target + drift - v) * 0.35 + (rnd() - 0.5) * 4;
  // Make the last hour gently rise into the range top for a realistic "now".
  if (now - t < 60 * MIN) v = Math.max(v, 130 + (60 - (now - t) / MIN) * 0.6);
  entries.push({ type: 'sgv', sgv: Math.round(Math.max(45, Math.min(380, v))), date: t, device: 'xDrip-DexcomG7' });
}
for (let i = 0; i < entries.length; i++) {
  const prev = entries[i - 1];
  const dlt = prev ? entries[i].sgv - prev.sgv : 0;
  entries[i].direction = dlt > 15 ? 'DoubleUp' : dlt > 9 ? 'SingleUp' : dlt > 4 ? 'FortyFiveUp' : dlt < -15 ? 'DoubleDown' : dlt < -9 ? 'SingleDown' : dlt < -4 ? 'FortyFiveDown' : 'Flat';
}
for (const e of events) {
  treatments.push({ eventType: 'Meal Bolus', carbs: e.carbs, insulin: Math.round(e.carbs / 10 * 20) / 20, created_at: new Date(e.at).toISOString(), enteredBy: 'Trio' });
}
treatments.push({ eventType: 'Note', notes: 'Football practice', created_at: new Date(now - 200 * MIN).toISOString(), enteredBy: 'Mum' });

(async () => {
  const post = async (path, body) => {
    const r = await fetch(BASE + path, { method: 'POST', headers: H, body: JSON.stringify(body) });
    if (!r.ok) throw new Error(path + ' ' + r.status + ' ' + (await r.text()));
  };
  for (let i = 0; i < entries.length; i += 500) await post('/api/v1/entries', entries.slice(i, i + 500));
  await post('/api/v1/treatments', treatments);
  // A basal profile and some temp basals, as a loop would upload.
  await post('/api/v1/profile', {
    defaultProfile: 'Default', startDate: new Date(start).toISOString(),
    store: { Default: {
      dia: 6, units: 'mg/dl', timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      basal: [{ time: '00:00', value: 0.6 }, { time: '04:00', value: 0.8 }, { time: '09:00', value: 0.7 }, { time: '18:00', value: 0.75 }, { time: '22:00', value: 0.6 }],
    } },
  });
  const temps = [];
  for (let t = now - 24 * 60 * MIN; t < now; t += 30 * MIN) {
    if (rnd() < 0.45) {
      temps.push({ eventType: 'Temp Basal', absolute: Math.round(rnd() * 16) / 10, rate: undefined, duration: 30, created_at: new Date(t).toISOString(), enteredBy: 'Trio' });
    }
  }
  await post('/api/v1/treatments', temps);

  // Forecasts in the openaps format (Trio, AndroidAPS): 5-minute steps from now.
  const last = entries[entries.length - 1].sgv;
  const curve = (n, slope, curveTo) => Array.from({ length: n }, (_, i) => Math.round(last + slope * i + (curveTo - last - slope * n) * (i / n) ** 2));
  await post('/api/v1/devicestatus', {
    device: 'Trio', created_at: new Date(now).toISOString(),
    openaps: {
      iob: { iob: 1.35 },
      suggested: {
        COB: 12, IOB: 1.35, timestamp: new Date(now).toISOString(),
        predBGs: { IOB: curve(36, 0.4, 118), COB: curve(36, 1.2, 172), UAM: curve(30, 1.6, 205), ZT: curve(30, -0.2, 120) },
      },
    },
    pump: { reservoir: 86, battery: { percent: 72 } }, uploader: { battery: 64 },
  });
  console.log(`seeded ${entries.length} entries, ${treatments.length} treatments`);
})().catch((e) => { console.error(e); process.exit(1); });
