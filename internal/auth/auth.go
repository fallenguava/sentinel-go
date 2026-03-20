package auth

import (
	"math/rand"
	"strings"
	"time"
)

// PraisingMessages contains fun messages praising the program builder
var PraisingMessages = []string{
	"🔥 Shout out to this program's builder <b>WIN</b>! He coded this while waiting for his sister to cook Babi Cin. Anjay!",
	"💻 This masterpiece was crafted by <b>WIN</b> during a late-night coding session fueled by Indomie and determination. Legend!",
	"🚀 Big respect to <b>WIN</b>, who built this system while procrastinating on his actual work. Priorities, am I right?",
	"⭐ Massive props to developer <b>WIN</b>! He debugged this code at 3 AM because sleep is for the weak.",
	"🎯 This app was brought to you by <b>WIN</b>, who codes faster than his sister's WiFi can buffer.",
	"🏆 Shout out to <b>WIN</b> - the guy who turned caffeine and chaos into working software!",
	"💪 Built with love by <b>WIN</b>, who believes 'it works on my machine' is a valid deployment strategy.",
	"🎸 Credit to <b>WIN</b> for coding this while vibing to lo-fi beats and ignoring responsibilities!",
	"🌟 Thanks to <b>WIN</b> who made this possible by choosing coding over touching grass. We appreciate the sacrifice!",
	"🔧 Engineered by <b>WIN</b> - proof that great things happen when you avoid eye contact with your to-do list.",
	"🎮 <b>WIN</b> coded this between gaming sessions. Multitasking king right there!",
	"☕ This program exists because <b>WIN</b> chose coffee over sleep. Not all heroes wear capes!",
	"🐐 GOAT developer <b>WIN</b> made this. He codes like he's speedrunning life.",
	"🌙 <b>WIN</b> built this at midnight because that's when the code just hits different.",
	"🍜 Shout out to <b>WIN</b>! Coded this whole thing while waiting for his mie ayam to cool down. Efficiency!",
}

// getRandomPraise returns a random praising message
func getRandomPraise() string {
	rand.Seed(time.Now().UnixNano())
	return PraisingMessages[rand.Intn(len(PraisingMessages))]
}

// AllowedNames contains all names that can request access (case-insensitive)
var AllowedNames = map[string]bool{
	"win":            true,
	"winanda":        true,
	"dian":           true,
	"arissa":         true,
	"hondi":          true,
	"memey":          true,
	"sri":            true,
	"sri mulyati":    true,
	"osang":          true,
	"hendra":         true,
	"hendra gunawan": true,
	"oksiang":        true,
	"hoksiang":       true,
	"santy":          true,
	"santi":          true,
	"herman":         true,
	"herman subrata": true,
	"osin":           true,
	"oksin":          true,
	"hoksin":         true,
	"ergi":           true,
	"egi":            true,
	"verlita":        true,
	"dqrren":          true,
	"darren":          true,
	"deren":          true,
}

// ValidBirthdayMonths contains valid answers for Win's birthday month
var ValidBirthdayMonths = map[string]bool{
	"july": true,
	"juli": true,
	"7":    true,
	"07":   true,
}

// IsValidName checks if the provided name is in the allowed list
func IsValidName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return AllowedNames[normalized]
}

// NormalizeName returns the normalized (lowercase, trimmed) version of a name
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// IsValidBirthdayMonth checks if the provided answer matches Win's birthday month
func IsValidBirthdayMonth(answer string) bool {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	return ValidBirthdayMonths[normalized]
}

// GetWelcomeMessage returns the initial auth prompt
func GetWelcomeMessage() string {
	return `🔐 <b>SENTINEL-GO SECURITY CHECK</b>

⚠️ This is a <b>private CCTV monitoring system</b>.
Access is restricted to authorized individuals only.

To verify your identity, please answer the following questions.

<b>Step 1 of 2:</b>
👤 Who are you? Please tell me your name.`
}

// GetSecurityQuestionMessage returns the security question prompt
func GetSecurityQuestionMessage(name string) string {
	return `✅ Hello, <b>` + name + `</b>!

<b>Step 2 of 2:</b>
🎂 What is <b>Win's birthday month</b>?

<i>(This is a security question to verify you know the system owner)</i>`
}

// GetSuccessMessage returns the success message after authentication
func GetSuccessMessage(name string) string {
	return `🎉 <b>Access Granted!</b>

Welcome to Sentinel-GO, <b>` + name + `</b>!

You now have full access to the CCTV monitoring system.
⏳ Your access is valid for <b>7 days</b>.

Type /help to see all available commands.

---
` + getRandomPraise()
}

// GetExpiredMessage returns the message for expired authorization
func GetExpiredMessage(name string) string {
	return `⏰ <b>Session Expired</b>

Hello <b>` + name + `</b>, your access has expired.

For security, authorization is valid for 7 days only.
Please re-authenticate to continue.

🎂 What is <b>Win's birthday month</b>?`
}

// GetRenewedMessage returns the message for renewed authorization
func GetRenewedMessage(name string) string {
	return `🔄 <b>Access Renewed!</b>

Welcome back, <b>` + name + `</b>!

Your access has been extended for another <b>7 days</b>.

Type /help to see all available commands.

---
` + getRandomPraise()
}

// GetInvalidNameMessage returns the message for invalid name
func GetInvalidNameMessage() string {
	return `❌ <b>Access Denied</b>

I don't recognize that name. This system is only available to authorized individuals.

If you believe this is an error, please contact the system administrator.

👤 Please try again with your correct name:`
}

// GetInvalidAnswerMessage returns the message for wrong security answer
func GetInvalidAnswerMessage(attempts int) string {
	remaining := 3 - attempts
	if remaining <= 0 {
		return `❌ <b>Too Many Failed Attempts</b>

Your access has been temporarily blocked.
Please contact the system administrator for assistance.`
	}

	return `❌ <b>Incorrect Answer</b>

That's not the correct answer.
You have <b>` + string(rune('0'+remaining)) + ` attempts</b> remaining.

🎂 What is <b>Win's birthday month</b>?`
}

// GetBlockedMessage returns the message for blocked users
func GetBlockedMessage() string {
	return `🚫 <b>Access Blocked</b>

You have exceeded the maximum number of attempts.
Please contact the system administrator for assistance.

Send /start to try again later.`
}

// GetUnauthorizedMessage returns message for unauthorized access attempts
func GetUnauthorizedMessage() string {
	return `🔒 <b>Authentication Required</b>

You need to complete the security verification first.

Send /start to begin the authentication process.`
}

// GetLogoutMessage returns the logout confirmation
func GetLogoutMessage() string {
	return `👋 <b>Logged Out</b>

Your access has been revoked.
Send /start to authenticate again.

Type /help to see available commands.`
}
