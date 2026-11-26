package store

import "strings"

// DefaultFreeMailDomains are the public mailbox providers whose addresses
// say nothing about an employer. An email identity on any other domain is
// stored local_only, whatever locality the caller asks for. Consumers may
// replace the list with Options.FreeMailDomains.
var DefaultFreeMailDomains = []string{
	"163.com",
	"aol.com",
	"fastmail.com",
	"gmail.com",
	"gmx.com",
	"gmx.de",
	"gmx.net",
	"googlemail.com",
	"hey.com",
	"hotmail.com",
	"icloud.com",
	"live.com",
	"mac.com",
	"mail.com",
	"mail.ru",
	"me.com",
	"msn.com",
	"outlook.com",
	"pm.me",
	"proton.me",
	"protonmail.com",
	"qq.com",
	"tutanota.com",
	"users.noreply.github.com",
	"web.de",
	"yahoo.com",
	"yandex.com",
	"yandex.ru",
	"zoho.com",
}

type freeMail map[string]bool

func newFreeMail(domains []string) freeMail {
	if domains == nil {
		domains = DefaultFreeMailDomains
	}
	fm := freeMail{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			fm[d] = true
		}
	}
	return fm
}

// has reports whether email is on a free-mail domain. A GitHub noreply
// address ("123+login@users.noreply.github.com") counts as free-mail.
func (fm freeMail) has(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	return fm[strings.ToLower(strings.TrimSpace(email[at+1:]))]
}
