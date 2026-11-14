package auth

import (
	"net/http"
)

// DeniedSpans charges n spans against the request's key and, when they are over
// the limit, answers 429 and says so. A refused request gives back the request
// and byte tokens the gate already took, so that a sender retrying after a spans
// refusal does not also run itself into the request and byte limits, whose
// rejections would then hide the real cause. With auth off there is no key and it
// never denies.
func DeniedSpans(w http.ResponseWriter, r *http.Request, n int) bool {
	p := FromContext(r.Context())
	ok, retry := p.AllowSpans(n)
	if ok {
		return false
	}
	p.refund(chargedSize(r))
	Refuse(w, retry)
	return true
}

// DeniedLogBytes is DeniedSpans for the day's log-byte allowance.
func DeniedLogBytes(w http.ResponseWriter, r *http.Request, n int) bool {
	p := FromContext(r.Context())
	ok, retry := p.AllowLogBytes(n)
	if ok {
		return false
	}
	p.refund(chargedSize(r))
	Refuse(w, retry)
	return true
}

func chargedSize(r *http.Request) int64 {
	n, _ := r.Context().Value(sizeKey{}).(int64)
	return n
}
