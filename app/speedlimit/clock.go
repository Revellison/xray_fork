package speedlimit

import "time"

// Clock — монотонное время в наносекундах. Нужен, чтобы лесенку штрафов можно
// было тестировать без реального ожидания.
type Clock interface{ NowNanos() int64 }

var processStart = time.Now()

type realClock struct{}

func (realClock) NowNanos() int64 { return int64(time.Since(processStart)) } // монотонное
