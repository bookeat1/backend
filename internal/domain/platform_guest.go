package domain

import (
	"time"

	"github.com/google/uuid"
)

// PLATFORM GUEST LIST — «кто вообще есть у платформы».
//
// Гость здесь — это ЧЕЛОВЕК, а не строка в users: половина броней сделана без
// регистрации, и такой человек всё равно гость, которому платформа однажды
// захочет что-то отправить. Поэтому идентичность строки — нормализованный
// телефон, тот же, по которому уже собирается список гостей ОДНОГО заведения
// (см. infrastructure/postgres/guest). Аккаунт, если он есть, подклеивается к
// телефону, а не наоборот.
//
// Из этого следуют две вещи, которые видно в полях ниже:
//   - у человека может НЕ быть аккаунта (UserID == nil), но быть брони;
//   - у человека может быть аккаунт и НОЛЬ броней — это «зарегистрировался и
//     не дошёл», ровно тот сегмент, ради которого список и понадобился.

// PlatformGuest — одна строка платформенного списка гостей.
type PlatformGuest struct {
	// Phone — нормализованный номер (E.164), он же идентичность строки.
	Phone string
	// UserID — аккаунт, если человек регистрировался. nil у гостя, который
	// бронировал без регистрации.
	UserID *uuid.UUID
	// Name — имя из аккаунта, а если аккаунта нет — имя с последней брони.
	Name  string
	Email string
	// City и Language берутся из аккаунта; пустые у гостя без регистрации.
	City     string
	Language string
	// RegisteredAt — когда завёл аккаунт. nil, если аккаунта нет.
	RegisteredAt *time.Time

	BookingsCount  int
	VisitsCount    int
	CancelledCount int
	NoShowCount    int
	// VenuesCount — в скольких РАЗНЫХ заведениях человек бронировал: гость трёх
	// заведений и гость одного — это разные истории для рассылки.
	VenuesCount    int
	FirstBookingAt *time.Time
	LastBookingAt  *time.Time
}

// PlatformGuestSegment — готовый срез списка. Сегменты названы по вопросу, на
// который отвечают, а не по SQL: «кто дошёл», «кто не дошёл», «кто отменял».
type PlatformGuestSegment string

const (
	// GuestSegmentAll — все, кого платформа знает: и аккаунты, и брони.
	GuestSegmentAll PlatformGuestSegment = "all"
	// GuestSegmentRegistered — есть аккаунт (с бронями или без).
	GuestSegmentRegistered PlatformGuestSegment = "registered"
	// GuestSegmentBooked — есть хотя бы одна бронь. Это и есть «реальные
	// гости» в отличие от просто зарегистрировавшихся.
	GuestSegmentBooked PlatformGuestSegment = "booked"
	// GuestSegmentVisited — хотя бы раз дошёл (arrived/completed).
	GuestSegmentVisited PlatformGuestSegment = "visited"
	// GuestSegmentNeverVisited — бронировал, но ни разу не дошёл.
	GuestSegmentNeverVisited PlatformGuestSegment = "never_visited"
	// GuestSegmentNoBookings — зарегистрировался и ни разу не бронировал.
	GuestSegmentNoBookings PlatformGuestSegment = "no_bookings"
	// GuestSegmentCancelled — хотя бы раз отменял бронь.
	GuestSegmentCancelled PlatformGuestSegment = "cancelled"
)

// Valid сообщает, знаком ли сегмент. Неизвестное значение — ошибка ввода, а не
// повод молча показать всех: «фильтр не сработал» должно быть видно.
func (s PlatformGuestSegment) Valid() bool {
	switch s {
	case GuestSegmentAll, GuestSegmentRegistered, GuestSegmentBooked,
		GuestSegmentVisited, GuestSegmentNeverVisited, GuestSegmentNoBookings,
		GuestSegmentCancelled:
		return true
	default:
		return false
	}
}

// PlatformGuestSort — порядок строк.
type PlatformGuestSort string

const (
	// GuestSortLastBooking — по последней брони, свежие сверху. Умолчание:
	// список открывают, чтобы увидеть живых.
	GuestSortLastBooking PlatformGuestSort = "last_booking"
	GuestSortBookings    PlatformGuestSort = "bookings"
	GuestSortRegistered  PlatformGuestSort = "registered"
)

func (s PlatformGuestSort) Valid() bool {
	switch s {
	case GuestSortLastBooking, GuestSortBookings, GuestSortRegistered:
		return true
	default:
		return false
	}
}

// PlatformGuestQuery — фильтры списка. Все поля опциональны; пустой запрос
// возвращает всех, отсортированных по последней брони.
type PlatformGuestQuery struct {
	// Search — подстрока имени, телефона или почты, без учёта регистра.
	Search  string
	Segment PlatformGuestSegment
	// City фильтрует по городу АККАУНТА: у гостя без регистрации города нет,
	// и он из выборки по городу выпадает — это честнее, чем угадывать город по
	// заведению, где он бронировал.
	City string
	// RegisteredFrom/To — окно регистрации аккаунта.
	RegisteredFrom, RegisteredTo *time.Time
	// BookedFrom/To — окно ПОСЛЕДНЕЙ брони.
	BookedFrom, BookedTo *time.Time
	// MinBookings отсекает по числу броней (0 — не фильтровать).
	MinBookings int
	Language    string

	Sort    PlatformGuestSort
	Page    int
	PerPage int
}
