// Команда server — точка входа сервиса коротких ссылок.
//
// main намеренно минимальный: вся сборка зависимостей живёт в internal/app.
// Так main остаётся тестируемым «на глаз», а логику запуска можно
// переиспользовать (например, в интеграционных тестах).
package main

import (
	"log"

	"github.com/hokagedno/urlshortener/internal/app"
)

func main() {
	if err := app.Run(); err != nil {
		log.Fatalf("фатальная ошибка: %v", err)
	}
}
