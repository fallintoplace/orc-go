package orc

import "time"

// CalendarKind identifies the calendar used to encode historical dates and
// timestamps in an ORC file.
type CalendarKind uint32

const (
	// CalendarKindUnknown means that the ORC file does not name a calendar.
	CalendarKindUnknown CalendarKind = iota
	// CalendarKindJulianGregorian uses the Julian calendar before October 1582
	// and the Gregorian calendar from October 15, 1582 onward.
	CalendarKindJulianGregorian
	// CalendarKindProlepticGregorian uses the Gregorian calendar for all dates.
	CalendarKindProlepticGregorian
)

const (
	unixEpochJulianDayNumber = int64(2440588)
	calendarCutoverDay       = int64(-141427) // 1582-10-15 in the proleptic Gregorian calendar.
)

func supportedCalendarKind(kind CalendarKind) bool {
	return kind <= CalendarKindProlepticGregorian
}

func effectiveCalendarKind(fileCalendar, defaultCalendar CalendarKind) CalendarKind {
	if fileCalendar != CalendarKindUnknown {
		return fileCalendar
	}
	if defaultCalendar != CalendarKindUnknown {
		return defaultCalendar
	}
	// ORC readers historically interpret files without calendar metadata using
	// the Julian/Gregorian calendar.
	return CalendarKindJulianGregorian
}

func dateFromORCDays(days int64, calendar CalendarKind) time.Time {
	if calendar != CalendarKindJulianGregorian || days >= calendarCutoverDay {
		return time.Unix(days*86400, 0).UTC()
	}
	year, month, day := julianDateFromEpochDays(days)
	// Go's proleptic calendar normalizes Julian-only dates such as February 29
	// in a Gregorian non-leap year to the next valid date.
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func timestampInProlepticCalendar(value time.Time, calendar CalendarKind) time.Time {
	value = value.UTC()
	year, month, day := value.Date()
	if calendar == CalendarKindJulianGregorian {
		dayNumber := time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Unix() / 86400
		if dayNumber < calendarCutoverDay {
			year, month, day = julianDateFromEpochDays(dayNumber)
		}
	}
	hour, minute, second := value.Clock()
	return time.Date(year, month, day, hour, minute, second, value.Nanosecond(), time.UTC)
}

func julianDateFromEpochDays(days int64) (int, time.Month, int) {
	julianDayNumber := days + unixEpochJulianDayNumber
	a := julianDayNumber + 32082
	b := floorDiv(4*a+3, 1461)
	c := a - floorDiv(1461*b, 4)
	d := floorDiv(5*c+2, 153)
	day := c - floorDiv(153*d+2, 5) + 1
	month := d + 3 - 12*floorDiv(d, 10)
	year := b - 4800 + floorDiv(d, 10)
	return int(year), time.Month(month), int(day)
}

func floorDiv(value, divisor int64) int64 {
	quotient := value / divisor
	if value%divisor < 0 {
		quotient--
	}
	return quotient
}
