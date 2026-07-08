package informatics

import "time"

// runsSource — источник страниц посылок (для тестируемости FetchNewRuns).
type runsSource interface {
	FetchRunsPage(userID, page int) (runs []Run, pageCount int, err error)
}

// pageDelay — пауза между запросами страниц одного аккаунта: суммарно не
// более одного запроса в секунду, чтобы не «класть» информатикс (НФТ-3).
// Переопределяется в тестах.
var pageDelay = time.Second

// FetchNewRuns — инкрементальная выгрузка (7.4): собрать посылки с
// id > max_run_id, идя по страницам от новых к старым; при первом опросе
// аккаунта (кеша нет) выкачиваются все страницы.
//
// Кеш здесь НЕ обновляется: возвращается newMax, который вызывающий фиксирует
// через CommitMaxRunID только после успешной обработки посылок — иначе сбой
// записи события между выгрузкой и матчингом навсегда терял бы посылку
// (id уже ниже сохранённого водяного знака, а дедупликации в БД ещё нет).
func FetchNewRuns(src runsSource, cache *Cache, userID int) (newRuns []Run, newMax int64, err error) {
	maxRunID, known := cache.MaxRunID(userID)
	page := 1
	sawOld := false
	newMax = maxRunID
	for {
		if page > 1 {
			time.Sleep(pageDelay)
		}
		runs, pageCount, err := src.FetchRunsPage(userID, page)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range runs {
			if known && r.ID <= maxRunID {
				sawOld = true
				break
			}
			newRuns = append(newRuns, r)
			if r.ID > newMax {
				newMax = r.ID
			}
		}
		if sawOld || page >= pageCount || len(runs) == 0 {
			break
		}
		page++
	}
	// Посылка с непромежуточным вердиктом (ещё тестируется / в очереди / ждёт
	// проверки), отправленная недавно: не двигаем водяной знак выше неё,
	// иначе её поздний OK никогда не будет выкачан. Ограничение по возрасту
	// защищает от вечного перекачивания из-за посылок, у которых вердикт так
	// и не станет окончательным; дубли исключает run_id в БД.
	for _, r := range newRuns {
		if r.Pending() && time.Since(r.CreateTime) < pendingHoldback && r.ID-1 < newMax {
			newMax = r.ID - 1
		}
	}
	return newRuns, newMax, nil
}

// pendingHoldback — сколько ждать вердикта посылки, прежде чем пустить
// водяной знак выше неё.
const pendingHoldback = 15 * time.Minute

// CommitMaxRunID фиксирует водяной знак аккаунта после успешной обработки.
func CommitMaxRunID(cache *Cache, userID int, newMax int64) error {
	maxRunID, known := cache.MaxRunID(userID)
	if newMax > maxRunID || !known {
		return cache.SetMaxRunID(userID, newMax)
	}
	return nil
}
