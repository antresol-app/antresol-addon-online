package strings

import . "github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"

// The app protocol of «Смотреть онлайн» (internal/api/app.go): the way's
// label, the steps and their rows, as a native app shows them.
func init() {
	Register(EN, map[string]string{
		"Онлайн":               "Online",
		"Найти и смотреть":     "Find and Watch",
		"Не найдено":           "Not found",
		"Источник":             "Source",
		"Озвучка":              "Audio",
		"Озвучка по умолчанию": "Default audio",
		"Сезон":                "Season",
		"Сезон %d":             "Season %d",
		"Серия":                "Episode",
		"Качество":             "Quality",
		"Вариант":              "Match",
		"Выберите подходящий вариант":      "Choose the right match",
		"Онлайн-источники ничего не нашли": "Online sources found nothing",
		"Субтитры":     "Subtitles",
		"Субтитры: %s": "Subtitles: %s",
	})
}
