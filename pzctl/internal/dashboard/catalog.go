package dashboard

// The catalogs. The Russian strings are v1's, character for character, because
// step 7's gate is a visual diff against the running dashboard and a reworded
// button is a diff that has to be explained. Where a message is new — the two
// player-count cases v1 could not express, and the backups page's download
// column — it is marked.

var catalog = map[Lang]text{
	RU: {
		PageTitle:  "Всрания • Хаб",
		Brand:      "Всрания • Хаб",
		NavConnect: "Подключение",
		NavBackups: "Бэкапы 🔒",

		ServerTitle:    "Статус сервера",
		ServerSubtitle: "Akash Network deployment",
		StatusOnline:   "ОНЛАЙН",
		StatusBooting:  "ЗАПУСК",
		StatusStopping: "ОСТАНОВКА",
		StatusOffline:  "ОФФЛАЙН",
		LabelIP:        "IP СЕРВЕРА",
		LabelHost:      "АДРЕС СЕРВЕРА",
		LabelPort:      "ПОРТ",
		LabelPassword:  "ПАРОЛЬ СЕРВЕРА",
		CopyAddress:    "Скопировать",
		CopyDone:       "Скопировано",
		LocationTitle:  "Расположение сервера",

		BannerBootingTitle:  "Сервер запускается",
		BannerBootingDesc:   "Инициализация инстанса на Akash Network. IP и порт появятся здесь автоматически после старта.",
		BannerStoppingTitle: "Сервер выключается",
		BannerStoppingDesc:  "Сохранение игрового мира, создание бэкапа и остановка инстанса. Скоро сервер перейдет в оффлайн.",
		BannerOfflineTitle:  "Сервер оффлайн",
		BannerOfflineDesc:   "Сервер выключен. Вы можете скачать клиент и моды ниже для подготовки к игре.",

		Players: Plural{One: "игрок", Few: "игрока", Many: "игроков"},
		// New in v2. v1 had no way to say either of these: it printed a
		// hardcoded 0 as "0 игроков", so "nobody is playing" and "nobody
		// asked the server" looked the same on the page.
		PlayersUnknown: "игроки: нет данных",
		PlayersStale:   "данные устарели",

		TorrentTitle: "🎮 Чистый клиент игры (.torrent)",
		TorrentDesc:  "Проверенный клиент, соответствующий версии сервера. Настоятельно рекомендуется удалить или переименовать папку Zomboid перед установкой.",
		TorrentBtn:   "Скачать .torrent",

		CardClientTitle:       "Файлы клиента",
		CardClientBtn:         "Скачать client.zip",
		CardCommonTitle:       "Общие файлы",
		CardCommonBtn:         "Скачать common.zip",
		CardServerTitle:       "Файлы сервера",
		CardServerBtnLocked:   "Разблокировать server.zip",
		CardServerBtnUnlocked: "Скачать server.zip",
		StatsMods:             Plural{One: "мод", Few: "мода", Many: "модов"},
		StatsFiles:            Plural{One: "файл", Few: "файла", Many: "файлов"},
		StatsReady:            "Готов",

		GuideTitle: "Инструкция по установке",

		ModalServerTitle:        "🔒 Файлы сервера",
		ModalServerDesc:         "Введите пароль для скачивания server.zip (конфигурации и серверные моды):",
		ModalServerPlaceholder:  "Пароль файлов сервера...",
		ModalBackupsPlaceholder: "Пароль бэкапов...",
		ModalCancel:             "Отмена",
		ModalUnlock:             "Разблокировать",
		ModalVerifying:          "Проверка...",
		ModalErrEmpty:           "Пожалуйста, введите пароль.",
		ModalErrWrong:           "Неверный пароль. Доступ запрещен.",

		BackupsPageTitle: "Всрания • Бэкапы",
		BackupsTitle:     "🗄️ Бэкапы игрового мира",
		BackupsSubtitle:  "Автоматические и ручные снимки, хранящиеся на контроллере",
		Archives:         Plural{One: "архив", Few: "архива", Many: "архивов"},
		ThName:           "ИМЯ АРХИВА",
		ThDate:           "ДАТА СОЗДАНИЯ",
		ThSize:           "РАЗМЕР",
		ThAction:         "ДЕЙСТВИЕ",
		Download:         "Скачать",
		// New in v2: the store records whether anyone ever fetched a copy, and
		// that is the only evidence a backup exists anywhere but on a disk that
		// dies with the lease.
		Downloaded:    "скачан",
		NotDownloaded: "копии нет",
		// v1 said "не найдены в /data/backups/". The path is configuration now,
		// and naming the controller's filesystem layout on a public page tells a
		// stranger more than it tells the operator.
		NoBackups:     "Архивы бэкапов не найдены",
		PwdRequired:   "Требуется пароль бэкапов",
		PwdDesc:       "Бэкапы и архивы мира защищены паролем.",
		WrongPwd:      "Неверный пароль бэкапов. Доступ запрещен.",
		UnlockBtn:     "Разблокировать",
		DiskWarning:   "Диск контроллера заполнен на %d%%. Скачайте бэкапы: старые будут удалены.",
		RestoreTarget: "будет восстановлен при следующем старте",

		UploadTitle:  "⬆️ Загрузить архив бэкапа",
		UploadDesc:   "Загрузите существующий архив сохранения мира .zip на контроллер:",
		UploadBtn:    "Загрузить .zip",
		UploadBusy:   "Загрузка...",
		UploadDone:   "Архив загружен",
		UploadFailed: "Не удалось загрузить архив",
	},

	EN: {
		PageTitle:  "Vsrania • Hub",
		Brand:      "Vsrania • Hub",
		NavConnect: "Connect",
		NavBackups: "Backups 🔒",

		ServerTitle:    "Server status",
		ServerSubtitle: "Akash Network deployment",
		StatusOnline:   "ONLINE",
		StatusBooting:  "STARTING UP",
		StatusStopping: "STOPPING",
		StatusOffline:  "OFFLINE",
		LabelIP:        "SERVER IP",
		LabelHost:      "SERVER ADDRESS",
		LabelPort:      "PORT",
		LabelPassword:  "SERVER PASSWORD",
		CopyAddress:    "Copy",
		CopyDone:       "Copied",
		LocationTitle:  "Server location",

		BannerBootingTitle:  "Vsrania is Starting Up",
		BannerBootingDesc:   "Initializing instance on Akash. IP and Port will appear here automatically when ready.",
		BannerStoppingTitle: "Vsrania is Shutting Down",
		BannerStoppingDesc:  "Saving game world, creating backup, and stopping instance. Server will be offline shortly.",
		BannerOfflineTitle:  "Vsrania is Offline",
		BannerOfflineDesc:   "Server is offline. You can download mods below in preparation for the session.",

		Players:        Plural{One: "player", Many: "players"},
		PlayersUnknown: "players: no data",
		PlayersStale:   "stale reading",

		TorrentTitle: "🎮 Clean Game Client (.torrent)",
		TorrentDesc:  "Pre-tested client matching server version. Recommended to delete or rename your existing Zomboid folder before install.",
		TorrentBtn:   "Download .torrent",

		CardClientTitle:       "Client Files",
		CardClientBtn:         "Download client.zip",
		CardCommonTitle:       "Common Files",
		CardCommonBtn:         "Download common.zip",
		CardServerTitle:       "Server Files",
		CardServerBtnLocked:   "Unlock server.zip",
		CardServerBtnUnlocked: "Download server.zip",
		StatsMods:             Plural{One: "mod", Many: "mods"},
		StatsFiles:            Plural{One: "file", Many: "files"},
		StatsReady:            "Ready",

		GuideTitle: "Quick Guide",

		ModalServerTitle:        "🔒 Unlock Server Files",
		ModalServerDesc:         "Enter password to download server.zip (server configs & mods):",
		ModalServerPlaceholder:  "Server files password...",
		ModalBackupsPlaceholder: "Backups password...",
		ModalCancel:             "Cancel",
		ModalUnlock:             "Unlock",
		ModalVerifying:          "Verifying...",
		ModalErrEmpty:           "Please enter a password.",
		ModalErrWrong:           "Incorrect password. Access denied.",

		BackupsPageTitle: "Vsrania • Backups",
		BackupsTitle:     "🗄️ World Save Backups",
		BackupsSubtitle:  "Automated and manual snapshots stored on the Controller",
		Archives:         Plural{One: "archive", Many: "archives"},
		ThName:           "ARCHIVE NAME",
		ThDate:           "CREATION DATE",
		ThSize:           "SIZE",
		ThAction:         "ACTION",
		Download:         "Download",
		Downloaded:       "downloaded",
		NotDownloaded:    "no copy",
		NoBackups:        "No backup archives found",
		PwdRequired:      "Backups Password Required",
		PwdDesc:          "Server backups and world archives are protected.",
		WrongPwd:         "Incorrect backups password. Access denied.",
		UnlockBtn:        "Unlock",
		DiskWarning:      "The controller's disk is %d%% full. Download your backups: the oldest are pruned.",
		RestoreTarget:    "restored on the next start",

		UploadTitle:  "⬆️ Upload Backup Archive",
		UploadDesc:   "Upload an existing world save .zip into the Controller:",
		UploadBtn:    "Upload .zip",
		UploadBusy:   "Uploading...",
		UploadDone:   "Archive uploaded",
		UploadFailed: "Could not upload the archive",
	},
}
