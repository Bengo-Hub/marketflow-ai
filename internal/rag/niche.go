package rag

import "strings"

// detectNiche infers businessType from question keywords when the client does not
// send an explicit business_type. Returns "flp", "codevertex", "cafe", or "generic".
// This is a fast heuristic — no LLM call, runs in microseconds.
func detectNiche(question string) string {
	q := strings.ToLower(question)

	// FLP / Forever Living keywords
	flpKW := []string{
		"forever living", "forever aloe", "aloe vera gel", "flp", "fbo",
		"forever business", "foreverliving", "forever2drive", "clean 9", "c9 challenge",
		"forever lite", "forever kids", "supergreens", "bee pollen", "royal jelly",
		"aloe lips", "tooth gel", "toothgel", "aloe vera juice", "forever therm",
		"arctic sea", "omega-3", "gelly", "propolis", "aloe activator",
		"sonya", "avocado soap", "aloe body lotion", "deodorant stick",
		"distributor id", "distrib id", "flp store", "flp shop",
		"join forever", "flp business", "mlm aloe", "network marketing aloe",
		"forever rank", "supervisor", "assistant manager", "senior manager",
		"sapphire manager", "diamond manager", "eagle manager",
	}
	for _, kw := range flpKW {
		if strings.Contains(q, kw) {
			return "flp"
		}
	}

	// Codevertex / platform keywords
	cvKW := []string{
		"codevertex", "codevertex africa", "digitika", "power suite", "marketflow", "marktflow",
		"erp", "pos system", "isp billing", "truload", "logistics app",
		"ordering app", "superset analytics", "books treasury",
		"ccna", "icdl", "software engineering course", "web dev course",
		"mobile dev course", "devops course", "cybersecurity course",
		"ai fundamentals", "ml with python", "full-stack",
		"pioneer house", "oginga street", "kisumu", "codevertexitsolutions",
		"accounts.codevertex", "erp.codevertex", "pos.codevertex",
	}
	for _, kw := range cvKW {
		if strings.Contains(q, kw) {
			return "codevertex"
		}
	}

	// Cafe / restaurant keywords
	cafeKW := []string{
		"menu", "food", "drink", "coffee", "espresso", "latte", "cappuccino",
		"brunch", "lunch", "dinner", "breakfast", "table reservation", "reserve a table",
		"book a table", "dining", "restaurant", "cafe", "bistro", "bakery",
		"pastry", "dessert", "cocktail", "wine", "beer", "smoothie",
		"opening hours", "opening time", "what time do you open", "what time do you close",
		"urban loft", "urbanloft",
	}
	for _, kw := range cafeKW {
		if strings.Contains(q, kw) {
			return "cafe"
		}
	}

	// Default: generic — neutral marketing assistant for any other business
	return "generic"
}

// scopeGuard checks whether a question is clearly outside the tenant's
// business scope before burning any LLM, embedding, or cache calls.
// Returns true when the question should be blocked as off-topic.
//
// Three layers:
//  1. Universal patterns — off-scope for every business niche
//  2. Niche-specific patterns — additional blocks per businessType
func scopeGuard(businessType, question string) bool {
	q := strings.ToLower(question)

	// Universal: explicit programming code requests are off-topic for ALL
	// business widgets. Vera is a support/sales assistant, not a code generator.
	universalBlocks := []string{
		"python code", "python script", "javascript code", "java code",
		"c++ code", "c# code", "php code", "ruby code", "golang code",
		"rust code", "swift code", "kotlin code", "typescript code",
		"show me code", "show me a function", "show me a script",
		"write a program", "write me a program", "sample code",
		"code snippet", "can i see code", "give me code",
		"programming tutorial", "coding tutorial",
		"sorting algorithm", "fibonacci sequence",
		"binary search algorithm", "recursion example",
		"import os", "import sys", "console.log(",
		"def main(", "public static void", "int main()",
	}
	for _, p := range universalBlocks {
		if strings.Contains(q, p) {
			return true
		}
	}

	switch businessType {
	case "flp":
		// FLP widgets are strictly health/wellness + FBO business assistants.
		// Block tech, general coding, academic homework, cooking, finance, etc.
		flpBlocks := []string{
			"programming", "write code", "coding", "software developer",
			"machine learning tutorial", "neural network tutorial",
			"html tutorial", "css tutorial", "react tutorial",
			"django tutorial", "flask tutorial", "api tutorial",
			"database design", "sql tutorial",
			"write my essay", "homework help", "exam prep help",
			"study guide for", "assignment help",
			"how to cook", "baking recipe", "ingredients for",
			"stock market tip", "forex signal", "crypto signal",
			"sports score", "movie review", "song lyrics",
		}
		for _, p := range flpBlocks {
			if strings.Contains(q, p) {
				return true
			}
		}

	case "cafe":
		cafeBlocks := []string{
			"write my essay", "do my homework", "homework help",
			"stock market tips", "crypto signals", "forex trading tips",
			"movie review", "celebrity gossip", "song lyrics",
			"programming", "write code", "coding tutorial",
		}
		for _, p := range cafeBlocks {
			if strings.Contains(q, p) {
				return true
			}
		}

	case "generic":
		genericBlocks := []string{
			"write my essay", "do my homework", "homework help",
			"stock market tips", "crypto signals", "forex trading tips",
			"movie review", "celebrity gossip", "song lyrics",
		}
		for _, p := range genericBlocks {
			if strings.Contains(q, p) {
				return true
			}
		}
	}

	return false
}

// offTopicMessage returns the niche-appropriate off-topic redirect message.
func offTopicMessage(businessType string) string {
	switch businessType {
	case "flp":
		return "I'm Vera, your Forever Living Products assistant. I can help with FLP products, health & wellness tips, or the FBO business opportunity — but that question is outside my expertise. What can I help you with today?"
	case "codevertex":
		return "I'm Vera, the Codevertex Africa Limited AI assistant. I can help with our IT services, Power Suite products, Digitika Academy courses, or technical questions. What would you like to know?"
	case "cafe":
		return "I'm Vera, your cafe assistant. I can help with the menu, upcoming events, table reservations, or anything about your dining experience — but that question is a bit outside my area. What can I help you with today?"
	default:
		return "I'm Vera, your business assistant. I can help with products, services, and business questions related to this company. What would you like to know?"
	}
}

func isGreeting(q string) bool {
	greetings := []string{
		"hi", "hello", "hey", "good morning", "good afternoon",
		"good evening", "thanks", "thank you", "bye", "goodbye",
		"hi!", "hello!", "hey!", "thanks!", "thank you!",
		"hi.", "hello.", "hey.", "sup", "yo", "howdy",
	}
	for _, g := range greetings {
		if q == g {
			return true
		}
	}
	return false
}
