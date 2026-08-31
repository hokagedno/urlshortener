// Package base62 кодирует целое число в короткую строку из 62 символов.
//
// Алгоритм: перевод числа в систему счисления с основанием 62.
// Сложность O(log₆₂ n) по времени и памяти — для int64 это максимум 11 символов.
//
// Почему так, а не случайная строка или хэш:
//   - гарантированная уникальность без проверок в БД (id из sequence уникален);
//   - обратимость (Decode) — полезно для отладки;
//   - нет коллизий, значит нет цикла «сгенерировал → проверил → повторил».
package base62

import (
	"errors"
	"strings"
)

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const base = int64(len(alphabet))

// ErrInvalidChar возвращается из Decode для символа вне алфавита.
var ErrInvalidChar = errors.New("base62: invalid character")

// index строится один раз при инициализации пакета,
// чтобы Decode работал за O(1) на символ вместо strings.IndexByte.
var index [256]int8

func init() {
	for i := range index {
		index[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		index[alphabet[i]] = int8(i)
	}
}

// Encode переводит неотрицательное число в base62-строку.
func Encode(n int64) string {
	if n == 0 {
		return string(alphabet[0])
	}
	if n < 0 {
		n = -n
	}
	// 11 символов достаточно для math.MaxInt64 в 62-ричной системе.
	buf := make([]byte, 0, 11)
	for n > 0 {
		buf = append(buf, alphabet[n%base])
		n /= base
	}
	// Цифры получились в обратном порядке — разворачиваем на месте.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// Decode переводит base62-строку обратно в число.
func Decode(s string) (int64, error) {
	if s == "" {
		return 0, ErrInvalidChar
	}
	var n int64
	for i := 0; i < len(s); i++ {
		d := index[s[i]]
		if d < 0 {
			return 0, ErrInvalidChar
		}
		n = n*base + int64(d)
	}
	return n, nil
}

// IsValid проверяет, что строка состоит только из символов алфавита.
func IsValid(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if index[s[i]] < 0 {
			return false
		}
	}
	return !strings.ContainsAny(s, " \t\n")
}
