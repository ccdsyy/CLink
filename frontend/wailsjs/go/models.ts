export namespace blacklist {
	
	export class Entry {
	    name: string;
	    reason?: string;
	    added_at: number;
	
	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.reason = source["reason"];
	        this.added_at = source["added_at"];
	    }
	}

}

export namespace main {
	
	export class NetCfg {
	    node: string;
	    mailbox: string;
	    stunMode: string;
	    stun: string;
	    turnNodes: string[];
	    turn: string;
	    localPort: number;
	    mcDir: string;
	    relayNodes: string;
	    relayPSK: string;
	    proxyUrl: string;
	
	    static createFrom(source: any = {}) {
	        return new NetCfg(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.node = source["node"];
	        this.mailbox = source["mailbox"];
	        this.stunMode = source["stunMode"];
	        this.stun = source["stun"];
	        this.turnNodes = source["turnNodes"];
	        this.turn = source["turn"];
	        this.localPort = source["localPort"];
	        this.mcDir = source["mcDir"];
	        this.relayNodes = source["relayNodes"];
	        this.relayPSK = source["relayPSK"];
	        this.proxyUrl = source["proxyUrl"];
	    }
	}

}

export namespace modhub {
	
	export class ModResult {
	    project_id: string;
	    slug: string;
	    title: string;
	    author: string;
	    description: string;
	    downloads: number;
	    icon_url: string;
	    categories: string[];
	
	    static createFrom(source: any = {}) {
	        return new ModResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.project_id = source["project_id"];
	        this.slug = source["slug"];
	        this.title = source["title"];
	        this.author = source["author"];
	        this.description = source["description"];
	        this.downloads = source["downloads"];
	        this.icon_url = source["icon_url"];
	        this.categories = source["categories"];
	    }
	}
	export class ModVersion {
	    version_id: string;
	    name: string;
	    version_number: string;
	    game_versions: string[];
	    loaders: string[];
	    date_published: string;
	    file_url: string;
	    filename: string;
	    file_size: number;
	
	    static createFrom(source: any = {}) {
	        return new ModVersion(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version_id = source["version_id"];
	        this.name = source["name"];
	        this.version_number = source["version_number"];
	        this.game_versions = source["game_versions"];
	        this.loaders = source["loaders"];
	        this.date_published = source["date_published"];
	        this.file_url = source["file_url"];
	        this.filename = source["filename"];
	        this.file_size = source["file_size"];
	    }
	}
	export class Progress {
	    id: string;
	    name: string;
	    dest: string;
	    done: number;
	    total: number;
	    speed: number;
	    threads: number;
	    finished: boolean;
	    failed: boolean;
	    err?: string;
	
	    static createFrom(source: any = {}) {
	        return new Progress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.dest = source["dest"];
	        this.done = source["done"];
	        this.total = source["total"];
	        this.speed = source["speed"];
	        this.threads = source["threads"];
	        this.finished = source["finished"];
	        this.failed = source["failed"];
	        this.err = source["err"];
	    }
	}

}

export namespace netprobe {
	
	export class Report {
	    hasV6: boolean;
	    v6Addrs: string[];
	    v4: boolean;
	    publicAddr: string;
	    natType: string;
	    tier: string;
	    stunNode: string;
	    rttMs: number;
	
	    static createFrom(source: any = {}) {
	        return new Report(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hasV6 = source["hasV6"];
	        this.v6Addrs = source["v6Addrs"];
	        this.v4 = source["v4"];
	        this.publicAddr = source["publicAddr"];
	        this.natType = source["natType"];
	        this.tier = source["tier"];
	        this.stunNode = source["stunNode"];
	        this.rttMs = source["rttMs"];
	    }
	}

}

export namespace toolbox {
	
	export class IPInfo {
	    ip: string;
	    region: string;
	    isp: string;
	    asn: string;
	
	    static createFrom(source: any = {}) {
	        return new IPInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ip = source["ip"];
	        this.region = source["region"];
	        this.isp = source["isp"];
	        this.asn = source["asn"];
	    }
	}
	export class MCServerStatus {
	    online: boolean;
	    players: string;
	    motd: string;
	    version: string;
	    latency_ms: number;
	
	    static createFrom(source: any = {}) {
	        return new MCServerStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.online = source["online"];
	        this.players = source["players"];
	        this.motd = source["motd"];
	        this.version = source["version"];
	        this.latency_ms = source["latency_ms"];
	    }
	}

}

export namespace updater {
	
	export class UpdateInfo {
	    currentVersion: string;
	    latestVersion: string;
	    currentBuild: number;
	    latestBuild: number;
	    notes: string;
	    download: string;
	    page: string;
	    hasUpdate: boolean;
	    fetchedAt: string;
	
	    static createFrom(source: any = {}) {
	        return new UpdateInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.currentVersion = source["currentVersion"];
	        this.latestVersion = source["latestVersion"];
	        this.currentBuild = source["currentBuild"];
	        this.latestBuild = source["latestBuild"];
	        this.notes = source["notes"];
	        this.download = source["download"];
	        this.page = source["page"];
	        this.hasUpdate = source["hasUpdate"];
	        this.fetchedAt = source["fetchedAt"];
	    }
	}

}

