"use strict";

const markets = {
  ETH: "WETH / USDC",
  BTC: "cbBTC / USDC",
  CBETH: "cbETH / USDC",
};
const usdcScale = 1_000_000n;
const elements = Object.fromEntries(
  [
    "market",
    "sort",
    "source",
    "summary",
    "period",
    "notice",
    "rows",
    "count",
    "status",
    "more",
    "reset",
    "detail",
  ].map((id) => [id, document.getElementById(id)]),
);
let cursor = "";
let records = [];
let listRequest = 0;
let detailRequest = 0;
let selected = "";

function amount(raw) {
  const value = BigInt(raw);
  const whole = (value / usdcScale).toLocaleString("en-US");
  const cents = ((value % usdcScale) / 10_000n).toString().padStart(2, "0");
  return `${whole}.${cents}`;
}

function escapeHTML(value) {
  return String(value).replace(
    /[&<>"']/g,
    (ch) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        ch
      ],
  );
}

function utc(timestamp, dateOnly = false) {
  const iso = new Date(timestamp * 1000).toISOString();
  return dateOnly
    ? iso.slice(0, 10)
    : `${iso.slice(0, 10)} ${iso.slice(11, 19)}`;
}

async function getJSON(url) {
  const response = await fetch(url);
  if (!response.ok)
    throw new Error(
      `Request failed (${response.status}). Reload to try again.`,
    );
  return response.json();
}

async function loadSummary() {
  try {
    const data = await getJSON("/api/summary");
    elements.source.textContent = data.synthetic
      ? "Synthetic sample dataset"
      : data.label;
    elements.notice.textContent = data.synthetic
      ? "The bundled dataset is synthetic. It demonstrates the interface; it does not report trading results."
      : data.label;
    elements.summary.innerHTML = `<div><strong>${data.blocks}</strong><span>Block records</span></div><div><strong>${data.positions}</strong><span>Liquidated positions</span></div>`;
    elements.period.textContent = `Dataset: ${utc(data.first, true)} to ${utc(data.last, true)} UTC`;
  } catch (err) {
    elements.source.textContent = "Dataset unavailable";
    elements.notice.textContent = err.message;
  }
}

function renderRows() {
  elements.rows.innerHTML = records
    .map((row) => {
      const key = `${row.market}:${row.block}`;
      return `<tr class="${selected === key ? "selected" : ""}">
      <td><button type="button" class="block-button" data-market="${row.market}" data-block="${row.block}" aria-pressed="${selected === key}" aria-label="Inspect ${markets[row.market]} block ${row.block}">${row.block.toLocaleString("en-US")}</button><span class="time">${utc(row.timestamp)}</span></td>
      <td><span class="market"><span class="dot ${row.market.toLowerCase()}" aria-hidden="true"></span>${markets[row.market]}</span></td>
      <td class="number">${row.positions}</td><td class="number">${amount(row.repaid_usdc)}</td></tr>`;
    })
    .join("");
  elements.count.textContent = `${records.length} loaded`;
}

async function loadActivity(append = false) {
  const request = ++listRequest;
  const params = new URLSearchParams({
    market: elements.market.value,
    sort: elements.sort.value,
  });
  if (append && cursor) params.set("cursor", cursor);
  if (!append) {
    ++detailRequest;
    records = [];
    selected = "";
    cursor = "";
    renderRows();
    elements.detail.textContent = "Select a block to inspect its liquidations.";
  }
  elements.more.disabled = true;
  elements.status.className = "status";
  elements.status.textContent = "Loading activity…";
  try {
    const page = await getJSON(`/api/activity?${params}`);
    if (request !== listRequest) return;
    records = append ? [...records, ...page.rows] : page.rows;
    cursor = page.next;
    renderRows();
    elements.status.textContent = records.length
      ? ""
      : "No activity in this market. Select another market.";
    if (!append && records.length)
      await selectBlock(records[0].market, records[0].block);
  } catch (err) {
    if (request !== listRequest) return;
    elements.status.className = "status error";
    elements.status.textContent = err.message;
  } finally {
    if (request === listRequest) elements.more.disabled = !cursor;
  }
}

async function selectBlock(market, block) {
  const request = ++detailRequest;
  selected = `${market}:${block}`;
  for (const button of elements.rows.querySelectorAll("button[data-block]")) {
    const active =
      `${button.dataset.market}:${button.dataset.block}` === selected;
    button.setAttribute("aria-pressed", String(active));
    button.closest("tr").classList.toggle("selected", active);
  }
  elements.detail.textContent = "Loading block details…";
  try {
    const row = await getJSON(
      `/api/block?${new URLSearchParams({ market, block })}`,
    );
    if (request !== detailRequest) return;
    const oracle =
      row.oracle_index < 0
        ? "No oracle update in this block"
        : `Oracle update at index ${row.oracle_index}`;
    const gap =
      row.oracle_index < 0
        ? ""
        : `<p class="order">The first liquidation appears <b>${Math.abs(row.liquidation_index - row.oracle_index)} transactions ${row.liquidation_index >= row.oracle_index ? "after" : "before"}</b> the oracle update.</p>`;
    elements.detail.innerHTML = `<p class="context">${markets[row.market]}</p><h2>Block ${row.block.toLocaleString("en-US")}</h2><p class="subheading">${utc(row.timestamp)} UTC</p>
      <dl><div><dt>Debt repaid</dt><dd>${amount(row.repaid_usdc)} USDC</dd></div><div><dt>Positions</dt><dd>${row.positions}</dd></div><div><dt>Liquidator</dt><dd>${escapeHTML(row.liquidator)}</dd></div></dl>
      <h3>Transaction order</h3><p class="order">${oracle}<br>First liquidation at index ${row.liquidation_index}</p>${gap}
      <h3>Liquidated positions</h3>${row.liquidations.map((p) => `<div class="position"><span>${escapeHTML(p.borrower)}</span><strong>${amount(p.repaid_usdc)} USDC</strong></div>`).join("")}`;
  } catch (err) {
    if (request === detailRequest) elements.detail.textContent = err.message;
  }
}

elements.market.addEventListener("change", () => loadActivity());
elements.sort.addEventListener("change", () => loadActivity());
elements.more.addEventListener("click", () => loadActivity(true));
elements.reset.addEventListener("click", () => loadActivity());
elements.rows.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-block]");
  if (button) selectBlock(button.dataset.market, Number(button.dataset.block));
});
loadSummary();
loadActivity();
