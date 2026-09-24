package kwaakaorders

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"backend-core/internal/domain"
)

// maxCommentRunes bounds the order comment.
// TODO(verify): Kwaaka has not told us the comment length limit (plan K10);
// 1000 is a conservative guess. On overflow dish comments go first, the
// "paid online" line is never cut.
const maxCommentRunes = 1000

// snapshotResult is what buildSnapshot returns.
type snapshotResult struct {
	Snapshot   domain.KitchenSnapshot
	TotalMinor int64
	Partial    bool // some dishes have no POS product id
	NoPosItems bool // nothing can be sent as a POS position
}

func fmtMoney(minor int64) string {
	if minor%100 == 0 {
		return fmt.Sprintf("%d", minor/100)
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// buildSnapshot makes the frozen order. Only pre-order lines and the guest's
// name/phone/guests go out — never booking notes, email, allergens or the
// foodie profile (PRD rule 28).
func buildSnapshot(s *domain.KitchenClaimSubject, orderID, tableID string, loc *time.Location) snapshotResult {
	res := snapshotResult{Snapshot: domain.KitchenSnapshot{OrderID: orderID, TableID: tableID,
		CustomerName: s.Name, CustomerPhone: s.Phone}}
	var dishNotes, manual []string
	for _, it := range s.Items {
		res.TotalMinor += it.PriceMinor * int64(it.Quantity)
		if it.KwaakaProductID != nil && *it.KwaakaProductID != "" {
			res.Snapshot.Items = append(res.Snapshot.Items, domain.KitchenSnapshotItem{
				ProductID: *it.KwaakaProductID, Name: it.Name, Quantity: it.Quantity,
				PriceMinor: it.PriceMinor, Currency: it.Currency})
			if it.Comment != nil && strings.TrimSpace(*it.Comment) != "" {
				dishNotes = append(dishNotes, it.Name+": "+strings.TrimSpace(*it.Comment))
			}
			continue
		}
		res.Partial = true
		manual = append(manual, fmt.Sprintf("%s ×%d — %s ₸", it.Name, it.Quantity, fmtMoney(it.PriceMinor*int64(it.Quantity))))
	}
	res.NoPosItems = len(res.Snapshot.Items) == 0

	code := s.BookingID.String()[:8]
	header := fmt.Sprintf("BookEat · бронь %s · %s · гостей %d · %s",
		code, s.StartsAt.In(loc).Format("02.01 15:04"), s.Guests, s.Name)
	pay := "Предзаказ НЕ оплачен"
	if s.Paid {
		pay = fmt.Sprintf("ОПЛАЧЕНО ОНЛАЙН %s ₸, повторно не брать", fmtMoney(s.PaidMinor))
	}
	lines := []string{header, pay}
	notesLine := ""
	if len(dishNotes) > 0 {
		notesLine = "К блюдам: " + strings.Join(dishNotes, "; ")
	}
	manualLine := ""
	if len(manual) > 0 {
		manualLine = "Нет в кассе, ввести вручную: " + strings.Join(manual, "; ")
	}
	join := func(withNotes bool) string {
		out := append([]string{}, lines...)
		if withNotes && notesLine != "" {
			out = append(out, notesLine)
		}
		if manualLine != "" {
			out = append(out, manualLine)
		}
		return strings.Join(out, "\n")
	}
	c := join(true)
	if utf8.RuneCountInString(c) > maxCommentRunes {
		c = join(false) // dish comments go first
	}
	if r := []rune(c); len(r) > maxCommentRunes {
		// still too long: cut the manual line's tail, keep header + pay line whole.
		c = string(r[:maxCommentRunes-1]) + "…"
	}
	res.Snapshot.Comment = c
	return res
}
