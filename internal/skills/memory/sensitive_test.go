package memory

import "testing"

func TestMentionsSensitive(t *testing.T) {
	for text, want := range map[string]bool{
		"you've been guarding Friday afternoons.":               false,
		"you've started walking at lunch, and exercise more":    false,
		"you've been painting on Sundays.":                      false,
		"you've been seeing a therapist on Thursdays.":          true,
		"you've been checking your bank balance a lot.":         true,
		"you've been quieter since the divorce.":                true,
		"you've stopped mentioning your ex.":                    true,
		"you keep a secret list of gift ideas.":                 true,
		"you've been spending less on takeaways.":               true,
		"you've moved £200 a week into savings.":                true,
		"your doctor's appointments are on Mondays.":            true,
		"your health has come up a lot.":                        true,
		"you've been talking about the wedding a lot.":          true,
		"you've been drinking less alcohol.":                    true,
		"you've been checking crypto prices every morning.":     true,
		"you've been sleeping badly and seem stressed.":         true,
		"you call your fiancée at lunch.":                       true,
		"you've been texting your fiance more.":                 true,
		"you love Saturdays with her.":                          true,
		"your lover rings on Tuesdays.":                         true,
		"you've been flirting on the train.":                    true,
		"sex has come up a few times.":                          true,
		"your family visits on Sundays.":                        true,
		"you've been drunk on Fridays.":                         true,
		"you've cut down on smoking.":                           true,
		"you can't sleep before midnight.":                      true,
		"your insomnia is back.":                                true,
		"you seem under stress.":                                true,
		"you're working through grief.":                         true,
		"you went to a funeral.":                                true,
		"since your cat died you work later.":                   true,
		"you go to the gym at six.":                             true,
		"weight-loss has come up a lot.":                        true,
		"you've been watching your stocks.":                     true,
		"you sold some shares.":                                 true,
		"you're not sure you can afford the trip.":              true,
		"you worry what the trip will cost.":                    true,
		"you think it's too expensive.":                         true,
		"you've been shopping at night.":                        true,
		"you've been buying a lot of books.":                    true,
		"you made a big purchase.":                              true,
		"your salaries come in on the 28th.":                    true,
		"you've started walking at lunch.":                      false,
		"you answer emails before nine and keep evenings free.": false,
	} {
		if got := MentionsSensitive(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}
