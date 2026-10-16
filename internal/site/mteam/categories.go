package mteam

// categoryNames maps M-Team category IDs to the simplified-Chinese names
// (nameChs) the API's torrent/categoryList returned on 2026-10-16. The table
// is static so a search costs no extra request; an ID missing here, such as
// one the site adds later, keeps its ID and gets no name rather than a guess.
// Some newer categories carry traditional-script names on the site itself;
// they are kept verbatim.
var categoryNames = map[string]string{
	"100": "电影",
	"105": "影剧/综艺",
	"110": "Music",
	"115": "AV(有码)",
	"120": "AV(无码)",
	"401": "电影/SD",
	"402": "影剧/综艺/HD",
	"403": "影剧/综艺/SD",
	"404": "纪录",
	"405": "动画",
	"406": "演唱",
	"407": "运动",
	"409": "Misc(其他)",
	"410": "AV(有码)/HD Censored",
	"411": "H-游戏",
	"412": "H-动漫",
	"413": "H-漫画",
	"419": "电影/HD",
	"420": "电影/DVDiSo",
	"421": "电影/BluRay",
	"422": "软件",
	"423": "PC游戏",
	"424": "AV(有码)/SD Censored",
	"425": "IV(写真影集)",
	"426": "AV(无码)/DVDiSo Uncensored",
	"427": "電子書",
	"429": "AV(无码)/HD Uncensored",
	"430": "AV(无码)/SD Uncensored",
	"431": "AV(有码)/Blu-Ray Censored",
	"432": "AV(无码)/Blu-Ray Uncensored",
	"433": "IV(写真图集)",
	"434": "Music(无损)",
	"435": "影剧/综艺/DVDiSo",
	"436": "AV(网站)/0Day",
	"437": "AV(有码)/DVDiSo Censored",
	"438": "影剧/综艺/BluRay",
	"439": "电影/Remux",
	"440": "AV(Gay)/HD",
	"442": "有聲書",
	"444": "紀錄",
	"445": "IV",
	"446": "H-ACG",
	"447": "遊戲",
	"448": "TV遊戲",
	"449": "動漫",
	"450": "其他",
	"451": "教育影片",
	"453": "动画/BluRay",
}

// categoryName returns the known display name for a canonical category ID.
func categoryName(id string) string {
	return categoryNames[id]
}
