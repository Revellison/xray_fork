package speedlimit

// window — скользящее окно из ≤60 корзин. Каждая корзина собирается из widthT тиков (1 тик = 1 с).
// Среднее считается только по ЗАПОЛНЕННЫМ корзинам: неполное окно не может вызвать срабатывание.
type window struct {
	n, widthT int
	buf       []uint64
	pos       int
	filled    int
	acc       uint64
	accT      int
}

func newWindow(forTicks int) *window {
	widthT := (forTicks + 59) / 60 // ceil(for/60), минимум 1
	if widthT < 1 {
		widthT = 1
	}
	n := (forTicks + widthT - 1) / widthT
	return &window{n: n, widthT: widthT, buf: make([]uint64, n)}
}

// feed добавляет прирост байтов за тик. Возвращает true, когда завершилась очередная корзина.
func (w *window) feed(delta uint64) bool {
	w.acc += delta
	w.accT++
	if w.accT < w.widthT {
		return false
	}
	w.buf[w.pos] = w.acc
	w.pos = (w.pos + 1) % w.n
	if w.filled < w.n {
		w.filled++
	}
	w.acc = 0
	w.accT = 0
	return true
}

// avgKbps возвращает среднюю скорость по ЗАПОЛНЕННЫМ корзинам в kbps. Если окно неполное, вернёт 0.
func (w *window) avgKbps() int64 {
	if w.filled < w.n {
		return 0
	}
	var sum uint64
	for i := 0; i < w.n; i++ {
		sum += w.buf[i]
	}
	totalSec := w.n * w.widthT
	if totalSec == 0 {
		return 0
	}
	bytesPerSec := sum / uint64(totalSec)
	return int64(bytesPerSec * 8 / 1000) // байт/с -> кбит/с
}

// reset очищает окно.
func (w *window) reset() {
	for i := range w.buf {
		w.buf[i] = 0
	}
	w.pos = 0
	w.filled = 0
	w.acc = 0
	w.accT = 0
}
