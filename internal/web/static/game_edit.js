// Форма создания/редактирования игры: динамика по n и списку команд.
"use strict";

const LEVEL_DEFAULTS = {
	task_cost: [12000, 12000, 12000, 12000, 12000, 12000],
	test_cost: [3000, 7000, 10000, 12000, 14000, 16000],
	load: [2, 2, 2, 2, 2, 2],
	amount_bonus: [12000, 25000, 50000, 75000, 110000, 160000],
	speed_bonus: [4, 7, 11, 15, 20, 26],
};

function syncLevels() {
	const n = Math.min(6, Math.max(2, parseInt(document.getElementById("n-input").value) || 3));
	const table = document.getElementById("levels-table");
	const rows = [...table.querySelectorAll("tr.level-row")];
	// Убрать лишние строки.
	for (const row of rows) {
		if (parseInt(row.dataset.level) > n) row.remove();
	}
	// Добавить недостающие (предзаполнены дефолтами из раздела 3.2).
	for (let lvl = rows.length + 1; lvl <= n; lvl++) {
		const tr = document.createElement("tr");
		tr.className = "level-row";
		tr.dataset.level = lvl;
		const i = Math.min(lvl - 1, 5);
		tr.innerHTML = `<td>${lvl}</td>` +
			["task_cost", "test_cost", "load", "amount_bonus", "speed_bonus"]
				.map((f) => `<td><input name="${f}_${lvl}" type="number" min="0" value="${LEVEL_DEFAULTS[f][i]}"></td>`)
				.join("");
		table.appendChild(tr);
	}
	// Текстарии задач.
	const box = document.getElementById("tasks-box");
	const taskRows = [...box.querySelectorAll(".task-row")];
	for (const row of taskRows) {
		if (parseInt(row.dataset.level) > n) row.remove();
	}
	for (let lvl = taskRows.length + 1; lvl <= n; lvl++) {
		const label = document.createElement("label");
		label.className = "task-row";
		label.dataset.level = lvl;
		label.innerHTML = `Уровень ${lvl}
			<textarea name="tasks_${lvl}" rows="4" cols="90"
				placeholder="https://informatics.msk.ru/mod/statements/view.php?chapterid=..."></textarea>`;
		box.appendChild(label);
	}
}

document.getElementById("n-input").addEventListener("change", syncLevels);

// Ручной (математический) режим: ссылки на задачи и informatics user_id
// не нужны — прячем блок задач и снимаем required-подсказки.
function syncMode() {
	const sel = document.getElementById("mode-input");
	if (!sel) return;
	const manual = sel.value === "manual";
	const box = document.getElementById("tasks-box");
	if (box) box.hidden = manual;
	const hint = document.getElementById("manual-tasks-hint");
	if (hint) hint.hidden = !manual;
	for (const inp of document.querySelectorAll('input[name="team_user_id"]')) {
		inp.disabled = manual;
		inp.placeholder = manual ? "не нужен" : "";
	}
}
const modeSel = document.getElementById("mode-input");
if (modeSel) {
	modeSel.addEventListener("change", syncMode);
	syncMode();
}

document.getElementById("add-team").addEventListener("click", () => {
	const table = document.getElementById("teams-table");
	const tr = document.createElement("tr");
	// team_id пустой = новая команда; поле обязано быть в каждой строке,
	// иначе параллельные массивы формы разъедутся.
	tr.innerHTML = `
		<td><input type="hidden" name="team_id" value=""><input name="team_name"></td>
		<td><input name="team_user_id" type="number" min="1"></td>
		<td><input name="team_login"></td>
		<td><input name="team_password"></td>
		<td><button type="button" class="secondary" onclick="this.closest('tr').remove()">×</button></td>`;
	table.appendChild(tr);
});
