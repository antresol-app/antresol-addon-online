package strings

import . "github.com/antresol-app/antresol-addon-online/internal/corecopy/i18n"

// Watch Online (internal/online): its refusals, statuses and errors. It
// left the core's catalogs with the code.
func init() {
	Register(EN, map[string]string{
		// online: Watch Online
		// the gate and TMDB (the core's catalogs have the same keys; the
		// addon registers its own)
		"Недоступно для этого профиля":                                 "Not available for this profile",
		"Неизвестный фильм или сериал":                                 "Unknown movie or TV show",
		"TMDB сейчас недоступен. Попробуйте позже":                     "TMDB is unavailable right now. Try again later",
		"Профиль не найден":                                            "Profile not found",
		"Недоступно в детском профиле":                                 "Not available in a kid profile",
		"Онлайн-источники сейчас недоступны":                           "Online sources are unavailable right now",
		"Источник не ответил":                                          "The source didn’t respond",
		"Ничего не нашлось в этом источнике":                           "Nothing found in this source",
		"Сейчас смотрят слишком много онлайн-видео. Попробуйте позже.": "Too many online videos are playing right now. Try again later.",
		"Этот формат не поддерживается, выберите другое качество":      "This format isn’t supported. Choose another quality",
		"Сеанс не найден":                                              "Session not found",
		"Слишком много одновременных потоков":                          "Too many simultaneous streams",
		"Серия %d": "Episode %d",
		// «Продолжить» (internal/hooks): the core's episode label
		"Сезон %d, серия %d": "Season %d, Episode %d",
		"Спецвыпуск %d":      "Special %d",
		"Неверная ссылка":    "Invalid link",
		// the routes (internal/api) and the start (main)
		"Расширение запускается":    "The addon is starting",
		"Расширение не готово":      "The addon is not ready",
		"Неверный запрос":           "Bad request",
		"Внутренняя ошибка сервера": "Internal server error",
	})
}
