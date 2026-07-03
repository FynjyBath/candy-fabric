// Отрисовка табло (публичного, командного и админского) + автообновление (8.4).
"use strict";

function initBoard(opts) {
	let clockOffsetMs = 0;   // server_time - Date.now()
	let lastState = null;
	let lastRendered = "";   // сериализованное состояние без server_time
	let errorStreak = 0;

	const timerEl = document.getElementById("timer");
	const offlineEl = document.getElementById("offline");

	function fmtHMS(totalSec) {
		if (totalSec < 0) totalSec = 0;
		const h = Math.floor(totalSec / 3600);
		const m = Math.floor((totalSec % 3600) / 60);
		const s = Math.floor(totalSec % 60);
		const p = (x) => String(x).padStart(2, "0");
		return `${p(h)}:${p(m)}:${p(s)}`;
	}

	function updateTimer() {
		if (!lastState) return;
		const st = lastState;
		const now = Date.now() + clockOffsetMs;
		if (!st.start_at) { timerEl.textContent = "Игра не началась"; return; }
		const start = Date.parse(st.start_at);
		const end = Date.parse(st.end_at);
		if (now < start) {
			timerEl.textContent = "До начала: " + fmtHMS((start - now) / 1000);
		} else if (now < end) {
			timerEl.textContent = "Оставшееся время: " + fmtHMS((end - now) / 1000);
		} else {
			timerEl.textContent = "Игра завершена";
		}
	}

	function num(x) { return x.toLocaleString("ru-RU"); }

	function cellDiv(cell, opts2) {
		const div = document.createElement("div");
		div.className = "cell " + cell.state;
		if (opts2 && opts2.link && cell.url) {
			const a = document.createElement("a");
			a.href = cell.url;
			a.target = "_blank";
			a.rel = "noopener";
			a.textContent = cell.cell;
			div.appendChild(a);
		} else {
			div.textContent = cell.cell;
		}
		if (cell.tests > 0 && cell.state === "bought") {
			const b = document.createElement("span");
			b.className = "tests-badge";
			b.textContent = cell.tests;
			div.appendChild(b);
		}
		return div;
	}

	function render(st) {
		lastState = st;
		updateTimer();

		// Колонки команд: имя, запасы, скорость, полоска скорости, сетка n×n.
		const teamsEl = document.getElementById("teams");
		teamsEl.textContent = "";
		for (const team of st.teams || []) {
			const col = document.createElement("div");
			col.className = "team-col";
			const h = document.createElement("h3");
			h.textContent = team.name;
			col.appendChild(h);
			const stats = document.createElement("div");
			stats.className = "stats";
			stats.innerHTML = `Запасы: <b>${num(team.amount)}</b><br>Скорость: <b>${num(team.speed)}</b>`;
			col.appendChild(stats);
			const stripe = document.createElement("div");
			stripe.className = "speed-stripe";
			// Период анимации обратно пропорционален скорости (декоративно).
			const speed = Math.max(team.speed, 0);
			stripe.style.animationDuration = speed > 0 ? Math.max(0.25, 16 / speed) + "s" : "0s";
			if (speed <= 0) stripe.style.animationPlayState = "paused";
			col.appendChild(stripe);
			const grid = document.createElement("div");
			grid.className = "grid";
			grid.style.gridTemplateColumns = `repeat(${st.n}, 1fr)`;
			for (const cell of team.cells || []) {
				const d = cellDiv(cell, { link: false });
				if (opts.mode === "admin") {
					d.classList.add("clickable");
					d.addEventListener("click", (ev) => adminCellMenu(ev, team, cell, st));
				}
				grid.appendChild(d);
			}
			col.appendChild(grid);
			teamsEl.appendChild(col);
		}

		// Гистограмма запасов: масштаб по лидеру (лидер = 100%).
		const histEl = document.getElementById("histogram");
		histEl.textContent = "";
		const maxAmount = Math.max(1, ...(st.teams || []).map((t) => t.amount));
		for (const team of st.teams || []) {
			const row = document.createElement("div");
			row.className = "bar-row";
			const label = document.createElement("div");
			label.className = "bar-label";
			label.textContent = team.name;
			const track = document.createElement("div");
			track.className = "bar-track";
			const bar = document.createElement("div");
			bar.className = "bar";
			bar.style.width = Math.max(0, (team.amount / maxAmount) * 100) + "%";
			bar.textContent = num(team.amount);
			track.appendChild(bar);
			row.appendChild(label);
			row.appendChild(track);
			histEl.appendChild(row);
		}

		// Таблица параметров уровней.
		const paramsEl = document.getElementById("params");
		paramsEl.textContent = "";
		const tbl = document.createElement("table");
		tbl.className = "list";
		tbl.innerHTML = "<tr><th>Уровень</th><th>Цена задачи</th><th>Цена подсказки</th><th>Нагрузка от задачи</th><th>Бонус запасы</th><th>Бонус скорость</th></tr>";
		(st.levels || []).forEach((l, i) => {
			const tr = document.createElement("tr");
			tr.innerHTML = `<td>${i + 1}</td><td>${num(l.task_cost)}</td><td>${num(l.test_cost)}</td>` +
				`<td>${num(l.load)}</td><td>${num(l.amount_bonus)}</td><td>${num(l.speed_bonus)}</td>`;
			tbl.appendChild(tr);
		});
		paramsEl.appendChild(tbl);

		// Собственная таблица задач команды (8.3): ссылки на условия,
		// порядок — перестановка этой команды, подписи строк — уровни.
		if (opts.mode === "team") {
			const own = (st.teams || []).find((t) => t.id === opts.teamId);
			const box = document.getElementById("own-table");
			if (own && box) {
				box.textContent = "";
				const n = st.n;
				const names = n === 3 ? ["Easy", "Middle", "Hard"] : [];
				const t = document.createElement("table");
				t.className = "list";
				for (let r = 0; r < n; r++) {
					const tr = document.createElement("tr");
					const th = document.createElement("th");
					th.textContent = "Уровень " + (r + 1) + (names[r] ? ` (${names[r]})` : "");
					tr.appendChild(th);
					for (let c = 0; c < n; c++) {
						const cell = own.cells[r * n + c];
						const td = document.createElement("td");
						td.appendChild(cellDiv(cell, { link: true }));
						tr.appendChild(td);
					}
					t.appendChild(tr);
				}
				box.appendChild(t);
			}
		}
	}

	async function tick() {
		try {
			const resp = await fetch(opts.stateUrl, { cache: "no-store" });
			if (!resp.ok) throw new Error("HTTP " + resp.status);
			let data = await resp.json();
			if (data.state) { // админский вариант: {state, poller_error}
				updatePollerBanner(data);
				data = data.state;
			}
			clockOffsetMs = Date.parse(data.server_time) - Date.now();
			errorStreak = 0;
			offlineEl.hidden = true;
			// Не перестраивать DOM, если состояние (кроме server_time) не менялось.
			const key = JSON.stringify({ ...data, server_time: null });
			if (key !== lastRendered) {
				lastRendered = key;
				render(data);
			} else {
				lastState = data;
				updateTimer();
			}
		} catch (e) {
			// Ошибка запроса — молча повторить; после 3 подряд — индикатор.
			errorStreak++;
			if (errorStreak >= 3) offlineEl.hidden = false;
		}
	}

	function updatePollerBanner(data) {
		const banner = document.getElementById("poller-banner");
		if (!banner) return;
		if (data.poller_error) {
			banner.hidden = false;
			document.getElementById("poller-error").textContent = data.poller_error;
			const at = new Date(data.poller_err_at);
			document.getElementById("poller-err-at").textContent = "(" + at.toLocaleString("ru-RU") + ")";
		} else {
			banner.hidden = true;
		}
	}

	tick();
	setInterval(tick, Math.max(1, opts.refreshSec) * 1000);
	setInterval(updateTimer, 1000);
}
