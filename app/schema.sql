PRAGMA foreign_keys = ON;
/*
func searchForImage(albumTitle, artist string) (discogsResp, error) {
	albumTitle = strings.TrimSpace(albumTitle)
	artist = strings.TrimSpace(artist)
	if albumTitle == "" || artist == "" {
		return discogsResp{}, fmt.Errorf("must provide strings for both albumTitle and artist")
	}
	// fmt resp for all three endpoints
	//https://api.discogs.com/database/search?release_title=a%20collection&artist=third%20eye%20blind&format=vinyl&per_page=1&page=1
	//"results" "master_id"
	//https://api.discogs.com/masters/594810
	//"uri" - discogs direct url
	//"genres" - array of styles by string
	//"styles" - array of styles by string
	//"images" [] - the entry with "type" : "primary", "uri" : "https://i.discogs.com/oauwCBqdKyEn_31qZQon5zHrKLSs4k8BDmUvEptK2SI/rs:fit/g:sm/q:90/h:600/w:600/czM6Ly9kaXNjb2dz/LWRhdGFiYXNlLWlt/YWdlcy9SLTMxMTcw/ODYtMTU0MjY3Njk1/OC03NDk2LmpwZWc.jpeg"
	return discogsResp{}, nil
}
*/

CREATE TABLE IF NOT EXISTS vinyl_unique(
	vinyl_id INTEGER PRIMARY KEY AUTOINCREMENT,
	vinyl_title TEXT NOT NULL,
	vinyl_artist TEXT NOT NULL,
	master_id INTEGER,
	styles TEXT, -- comma separated list of styles "Alternative Rock,Punk" etc
	genres TEXT, -- comma separated list of genres "Rock,Heavy Metal" etc TODO: need to make this take out '&' and whitespace
	UNIQUE (vinyl_title, vinyl_artist)
);

CREATE TABLE IF NOT EXISTS users(
	user_id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_name TEXT NOT NULL UNIQUE,
	date_created TEXT NOT NULL DEFAULT (date('now'))
);

CREATE TABLE IF NOT EXISTS vinyl_release(
	vinyl_id INTEGER NOT NULL,
	release_id INTEGER NOT NULL, -- from discogs api
	lowest_price REAL,
	price_last_updated TEXT, -- the date it gets updated/entered. "lowest_price"
	country TEXT,
	notes TEXT, -- discogs api
	released TEXT NOT NULL, 
	master_release INTEGER NOT NULL, -- 0 false 1 true
	resource_uri TEXT NOT NULL,
	image_extension TEXT NOT NULL,
	cover_raw_blob BLOB NOT NULL,
	cover_embedding BLOB NOT NULL,
	PRIMARY KEY (vinyl_id, release_id),
	FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS vinyl_plays(
	user_id INTEGER NOT NULL,
	vinyl_id INTEGER NOT NULL,
	release_id INTEGER NOT NULL,
	play INTEGER NOT NULL, -- 0 is 'owning' the vinyl
	played_date TEXT NOT NULL, -- DATE
	PRIMARY KEY (user_id, vinyl_id, release_id, play),
	FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE CASCADE,
	FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE,
	FOREIGN KEY (vinyl_id, release_id) REFERENCES vinyl_release(vinyl_id, release_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS vinyl_releases_check(
	vinyl_id INTEGER NOT NULL,
	release_id INTEGER NOT NULL, -- from discogs api
	label TEXT NOT NULL, -- from discogs api
	country TEXT NOT NULL,
	release_format TEXT NOT NULL,
	released_year INTEGER NOT NULL,
	cover_uri TEXT NOT NULL, -- from discogs api. used as a direct <img> source for releases we don't store in vinyl_release
	PRIMARY KEY (vinyl_id, release_id),
	FOREIGN KEY (vinyl_id) REFERENCES vinyl_unique(vinyl_id) ON DELETE CASCADE
);
