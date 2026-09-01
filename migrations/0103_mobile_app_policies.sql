-- +goose Up

-- ПОЛИТИКА ОБНОВЛЕНИЯ МОБИЛЬНОГО ПРИЛОЖЕНИЯ: РЕЖИМ РЕШАЕТ СЕРВЕР.
--
-- ЗАЧЕМ. Часть правок нельзя доставить по воздуху (expo-updates): всё, что
-- меняет нативную часть, едет только новой сборкой из магазина. До этой
-- таблицы попросить гостя обновиться было нечем — сборка в магазине не
-- спрашивала сервер ни о чём и жила ровно столько, сколько её не удалили.
--
-- ПОЧЕМУ ТАБЛИЦА, А НЕ ПЕРЕМЕННЫЕ ОКРУЖЕНИЯ. Порог «ниже этой версии
-- показывать блокирующий экран» меняют в момент инцидента, а не в момент
-- релиза: правка через .env — это заход по ssh, редактирование файла и
-- рестарт контейнера, то есть тот самый деплой, которого мы и пытаемся
-- избежать. Плюс тексты: их правит не инженер.
--
-- ПОЧЕМУ ОТДЕЛЬНАЯ ТАБЛИЦА, А НЕ ОБЩИЙ key-value «settings». Такой таблицы в
-- схеме нет, и заводить её ради одной фичи — значит превратить типизированные
-- настройки в набор строк без единой проверки на уровне базы. Схема здесь
-- везде идёт другим путём: у каждого платформенного справочника своя таблица
-- со своими ограничениями (cities, cuisines, venue_features, payment_providers,
-- restaurant_payout_settings). Набор строк закрытый (ios/android), поэтому
-- платформа — это PRIMARY KEY с CHECK, а не ключ в мешке строк.
--
-- ЧЕГО ЭТА МИГРАЦИЯ НЕ ДЕЛАЕТ: она НИКОГО не заставляет обновляться. Оба
-- порога засеяны пустыми строками, а пустой порог означает «порога нет»
-- (domain.MobileAppPolicy.Decide). Ручка с первого дня отвечает
-- action = "none" всем, пока порог не выставят руками через админскую ручку.

SET lock_timeout = '3s';

CREATE TABLE mobile_app_policies
(
    -- Платформа магазина. varchar + CHECK, а не CREATE TYPE ... AS ENUM —
    -- правило схемы (migrations/embed.go). Значения совпадают с
    -- domain.PlatformIOS / domain.PlatformAndroid.
    platform                varchar(16) PRIMARY KEY
        CHECK (platform IN ('ios', 'android')),

    -- Ниже этой версии — блокирующий экран. Пустая строка = никого не
    -- блокируем. Хранится СТРОКОЙ, как её ввёл оператор: разбор и сравнение
    -- живут в Go (domain.ParseAppVersion), и неразобранное значение читается
    -- как «порога нет», то есть опечатка может только не сработать, но не
    -- запереть гостя в приложении.
    min_supported_version   varchar(32)  NOT NULL DEFAULT '',
    -- Ниже этой версии — мягкое предложение обновиться.
    min_recommended_version varchar(32)  NOT NULL DEFAULT '',

    -- Куда ведёт кнопка «Обновить». Своя на каждую платформу.
    store_url               varchar(512) NOT NULL DEFAULT '',

    -- Тексты обоих режимов. Базовая колонка — русский, *_i18n — остальные
    -- языки: тот же инвариант, что у всех локализованных полей схемы
    -- (domain.ApplyTranslations, i18n['ru'] == колонка).
    recommended_title       varchar(256) NOT NULL DEFAULT '',
    recommended_title_i18n  jsonb,
    recommended_message     varchar(1024) NOT NULL DEFAULT '',
    recommended_message_i18n jsonb,
    required_title          varchar(256) NOT NULL DEFAULT '',
    required_title_i18n     jsonb,
    required_message        varchar(1024) NOT NULL DEFAULT '',
    required_message_i18n   jsonb,

    created_at              timestamptz  NOT NULL DEFAULT now(),
    updated_at              timestamptz  NOT NULL DEFAULT now()
);

-- Засев: обе платформы, ПОРОГИ ПУСТЫЕ.
--
-- Ссылки на магазины взяты из конфигурации самого приложения
-- (apps/mobile/app.config.js: ios.bundleIdentifier = com.bookeat.app,
-- android.package = kz.bookeat.app) и из подтверждённого ascAppId 6757542577.
-- Обе редактируются админской ручкой, если что-то из этого поменяется.
--
-- Тексты — заготовка, чтобы включение порога не требовало сначала придумать
-- слова. Правятся там же, без выкатки. Казахский вариант нуждается в вычитке
-- носителем: TODO(verify) — показать kk-тексты человеку до первого включения
-- required-режима.
INSERT INTO mobile_app_policies (platform, store_url,
                                 recommended_title, recommended_title_i18n,
                                 recommended_message, recommended_message_i18n,
                                 required_title, required_title_i18n,
                                 required_message, required_message_i18n)
VALUES ('ios',
        'https://apps.apple.com/app/id6757542577',
        'Доступно обновление',
        '{"ru":"Доступно обновление","kk":"Жаңарту қолжетімді","en":"Update available"}'::jsonb,
        'Вышла новая версия BookEat. Обновите приложение, чтобы всё работало как надо.',
        '{"ru":"Вышла новая версия BookEat. Обновите приложение, чтобы всё работало как надо.","kk":"BookEat қосымшасының жаңа нұсқасы шықты. Бәрі дұрыс жұмыс істеуі үшін қосымшаны жаңартыңыз.","en":"A new version of BookEat is out. Update the app to keep everything working."}'::jsonb,
        'Нужно обновить приложение',
        '{"ru":"Нужно обновить приложение","kk":"Қосымшаны жаңарту қажет","en":"Update required"}'::jsonb,
        'Эта версия BookEat больше не поддерживается. Обновите приложение, чтобы продолжить.',
        '{"ru":"Эта версия BookEat больше не поддерживается. Обновите приложение, чтобы продолжить.","kk":"BookEat қосымшасының бұл нұсқасы енді қолдау көрсетілмейді. Жалғастыру үшін қосымшаны жаңартыңыз.","en":"This version of BookEat is no longer supported. Please update the app to continue."}'::jsonb),
       ('android',
        'https://play.google.com/store/apps/details?id=kz.bookeat.app',
        'Доступно обновление',
        '{"ru":"Доступно обновление","kk":"Жаңарту қолжетімді","en":"Update available"}'::jsonb,
        'Вышла новая версия BookEat. Обновите приложение, чтобы всё работало как надо.',
        '{"ru":"Вышла новая версия BookEat. Обновите приложение, чтобы всё работало как надо.","kk":"BookEat қосымшасының жаңа нұсқасы шықты. Бәрі дұрыс жұмыс істеуі үшін қосымшаны жаңартыңыз.","en":"A new version of BookEat is out. Update the app to keep everything working."}'::jsonb,
        'Нужно обновить приложение',
        '{"ru":"Нужно обновить приложение","kk":"Қосымшаны жаңарту қажет","en":"Update required"}'::jsonb,
        'Эта версия BookEat больше не поддерживается. Обновите приложение, чтобы продолжить.',
        '{"ru":"Эта версия BookEat больше не поддерживается. Обновите приложение, чтобы продолжить.","kk":"BookEat қосымшасының бұл нұсқасы енді қолдау көрсетілмейді. Жалғастыру үшін қосымшаны жаңартыңыз.","en":"This version of BookEat is no longer supported. Please update the app to continue."}'::jsonb)
ON CONFLICT (platform) DO NOTHING;

-- +goose Down

-- Откат сносит ТОЛЬКО настройку показа баннера обновления. Бизнес-данных здесь
-- нет, ни один внешний ключ на таблицу не смотрит, поэтому DROP безопасен и на
-- живой базе. Единственная потеря — выставленные пороги и отредактированные
-- тексты; после наката обратно возвращается засев с ПУСТЫМИ порогами, то есть
-- откат гарантированно снимает принуждение, а не включает его.
DROP TABLE mobile_app_policies;
