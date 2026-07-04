// Мутации админки: меню ячейки, журнал событий, аномалии.
"use strict";

async function adminPost(url, fields) {
	const body = new URLSearchParams(fields);
	body.set("csrf", window.ADMIN.csrf);
	const resp = await fetch(url, {
		method: "POST",
		headers: { "Content-Type": "application/x-www-form-urlencoded" },
		body: body.toString(),
	});
	let data = {};
	try { data = await resp.json(); } catch (e) { /* не-JSON ответ */ }
	return { ok: resp.ok, data };
}

// Создание/изменение события с обработкой предупреждения «Недостаточно
// средств / производительности»: сохранение только после явного подтверждения.
async function postEventWithConfirm(url, fields) {
	let { ok, data } = await adminPost(url, fields);
	if (ok && data.warning) {
		if (!confirm(data.warning + ". Сохранить событие всё равно?")) return;
		fields.confirmed = "1";
		({ ok, data } = await adminPost(url, fields));
	}
	if (!ok) {
		alert("Ошибка: " + (data.error || "не удалось сохранить событие"));
		return;
	}
	location.reload();
}

function addEvent(fields) {
	postEventWithConfirm(`/admin/g/${window.ADMIN.gameId}/event`, fields);
}

// Меню ячейки живого табло: «Купить задачу» / «Купить тест» + информация о
// задаче (chapterid и ссылка) — админу можно.
function adminCellMenu(ev, team, cell, st) {
	closeCellMenu();
	const menu = document.createElement("div");
	menu.className = "cell-menu";
	menu.id = "cell-menu";

	// Собираем DOM без innerHTML: имя команды — пользовательский ввод.
	const title = document.createElement("div");
	const nameEl = document.createElement("b");
	nameEl.textContent = team.name;
	title.appendChild(nameEl);
	title.appendChild(document.createTextNode(`, ячейка ${cell.cell}`));
	title.appendChild(document.createElement("br"));
	if (cell.chapter_id) {
		title.appendChild(document.createTextNode("Задача: "));
		const a = document.createElement("a");
		a.href = cell.url;
		a.target = "_blank";
		a.rel = "noopener";
		a.textContent = `chapterid=${cell.chapter_id}`;
		title.appendChild(a);
		title.appendChild(document.createTextNode(` (уровень ${cell.level})`));
	} else {
		title.appendChild(document.createTextNode("Задача не назначена (игра не стартовала)"));
	}
	menu.appendChild(title);

	const buyTask = document.createElement("button");
	buyTask.textContent = "Купить задачу";
	buyTask.disabled = cell.state !== "hidden" || !cell.task_id;
	buyTask.onclick = () => addEvent({ team_id: team.id, task_id: cell.task_id, type: "buy_task" });
	menu.appendChild(buyTask);

	const buyTest = document.createElement("button");
	buyTest.textContent = "Купить тест";
	buyTest.disabled = cell.state !== "bought" || !cell.task_id;
	buyTest.onclick = () => addEvent({ team_id: team.id, task_id: cell.task_id, type: "buy_test" });
	menu.appendChild(buyTest);

	// Ручной зачёт: отметить решённой без посылки на информатиксе
	// (страховка на случай недоступности информатикса, ТЗ 5.2).
	const solve = document.createElement("button");
	solve.textContent = "Отметить решённой";
	solve.disabled = cell.state !== "bought" || !cell.task_id;
	solve.title = cell.state === "hidden" ? "сначала купите задачу" : "";
	solve.onclick = () => {
		if (!confirm(`Зачесть задачу как решённую команде «${team.name}» (без посылки на информатиксе)?`)) return;
		addEvent({ team_id: team.id, task_id: cell.task_id, type: "solve", comment: "зачтено вручную" });
	};
	menu.appendChild(solve);

	const close = document.createElement("button");
	close.className = "secondary";
	close.textContent = "Закрыть";
	close.onclick = closeCellMenu;
	menu.appendChild(close);

	document.body.appendChild(menu);
	const rect = ev.currentTarget.getBoundingClientRect();
	menu.style.left = window.scrollX + rect.left + "px";
	menu.style.top = window.scrollY + rect.bottom + 4 + "px";
	ev.stopPropagation();
	setTimeout(() => document.addEventListener("click", onDocClick), 0);
}

function onDocClick(e) {
	const menu = document.getElementById("cell-menu");
	if (menu && !menu.contains(e.target)) closeCellMenu();
}

function closeCellMenu() {
	const menu = document.getElementById("cell-menu");
	if (menu) menu.remove();
	document.removeEventListener("click", onDocClick);
}

// Формы журнала («Изменить», «Добавить событие»): везде один и тот же
// warning-флоу — и добавление, и редактирование могут увести в минус.
function submitEventForm(form, url) {
	const fields = {};
	for (const [k, v] of new FormData(form)) fields[k] = v;
	postEventWithConfirm(url, fields);
	return false;
}

function eventAction(id, action, confirmText) {
	if (confirmText && !confirm(confirmText)) return;
	adminPost(`/admin/g/${window.ADMIN.gameId}/event/${id}/${action}`, {}).then(({ ok, data }) => {
		if (!ok) { alert("Ошибка: " + (data.error || action)); return; }
		location.reload();
	});
}

function anomalyAction(id, action) {
	adminPost(`/admin/g/${window.ADMIN.gameId}/anomaly/${id}/${action}`, {}).then(({ ok, data }) => {
		if (!ok) { alert("Ошибка: " + (data.error || action)); return; }
		location.reload();
	});
}

// Фильтры журнала по команде и типу.
function initEventFilters() {
	const teamSel = document.getElementById("filter-team");
	const typeSel = document.getElementById("filter-type");
	if (!teamSel || !typeSel) return;
	const apply = () => {
		for (const tr of document.querySelectorAll("#events-table tr[data-team]")) {
			const okTeam = !teamSel.value || tr.dataset.team === teamSel.value;
			const okType = !typeSel.value || tr.dataset.type === typeSel.value;
			tr.style.display = okTeam && okType ? "" : "none";
		}
	};
	teamSel.addEventListener("change", apply);
	typeSel.addEventListener("change", apply);
}
