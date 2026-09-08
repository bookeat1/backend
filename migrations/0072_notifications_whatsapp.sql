-- +goose Up

-- WHATSAPP КАК ТРЕТИЙ КАНАЛ УВЕДОМЛЕНИЙ ЗАВЕДЕНИЮ.
--
-- WHY. Уведомление о новой броне с кнопками «подтвердить» / «отклонить» уже
-- работает в телеграме (0039), но в Казахстане рестораны живут в WhatsApp, а
-- не в телеграме. Механика решения при этом ОДНА И ТА ЖЕ — usecase bookings
-- ничего не знает про канал, — поэтому здесь появляется только адрес доставки,
-- а не вторая копия логики подтверждения.
--
-- Почему отдельная колонка, а не переиспользование restaurants.phone: телефон
-- заведения — это номер для гостей, по нему звонят. Номер, на который приходят
-- уведомления, может быть личным номером управляющего и вообще не совпадать с
-- публичным. Класть их в одну колонку значит однажды показать гостю чужой
-- личный номер.
--
-- whatsapp_enabled по умолчанию true, как и telegram_enabled: канал молчит,
-- пока не задан номер, поэтому «включён без адреса» — безопасное состояние, а
-- выключатель нужен для «номер есть, но временно не шлите».

ALTER TABLE restaurant_notification_settings
    ADD COLUMN whatsapp_phone   text,
    ADD COLUMN whatsapp_enabled boolean NOT NULL DEFAULT true;

-- Обратный поиск «из какого номера пришло нажатие» — это АВТОРИЗАЦИЯ входящего
-- вебхука, ровно как чат в телеграме: нажатие не несёт ни токена, ни аккаунта,
-- и единственное, чем оно доказывает своё право, — номер отправителя. Индекс
-- частичный: пустые номера в этот поиск попадать не должны, иначе первый же
-- вебхук с неизвестного номера совпал бы с заведением, у которого номер не
-- заполнен.
CREATE UNIQUE INDEX idx_restaurant_notification_whatsapp
    ON restaurant_notification_settings (whatsapp_phone)
    WHERE whatsapp_phone IS NOT NULL AND whatsapp_phone <> '';

-- +goose Down

DROP INDEX idx_restaurant_notification_whatsapp;
ALTER TABLE restaurant_notification_settings
    DROP COLUMN whatsapp_enabled,
    DROP COLUMN whatsapp_phone;
