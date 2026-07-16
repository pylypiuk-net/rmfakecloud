import { useEffect, useState } from "react";
import apiservice from "../../services/api.service";

const Home = () => {
  const [stats, setStats] = useState({
    totalDocs: 0,
    folders: 0,
    notebooks: 0,
    trashCount: 0,
    users: 0,
  });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const loadStats = async () => {
      try {
        const { Trash = [], Entries = [] } = await apiservice.listDocument();

        const countItems = (items) => {
          let docs = 0, folders = 0, notebooks = 0;
          const walk = (nodes) => {
            for (const n of nodes) {
              if (n.isFolder) {
                folders++;
                if (n.children) walk(n.children);
              } else {
                docs++;
                if (n.name && n.name.toLowerCase().includes("notebook")) notebooks++;
              }
            }
          };
          walk(items);
          return { docs, folders, notebooks };
        };

        const { docs, folders, notebooks } = countItems(Entries);
        setStats({
          totalDocs: docs,
          folders,
          notebooks,
          trashCount: Trash.length,
          users: 1, // current user
        });
      } catch (e) {
        console.error("Failed to load stats:", e);
      } finally {
        setLoading(false);
      }
    };
    loadStats();
  }, []);

  const cards = [
    {
      title: "Documents",
      value: stats.totalDocs,
      icon: "fa-file-alt",
      color: "#58a6ff",
      subtitle: `${stats.notebooks} notebooks`,
    },
    {
      title: "Folders",
      value: stats.folders,
      icon: "fa-folder",
      color: "#fbbf24",
      subtitle: "Organized collections",
    },
    {
      title: "Trash",
      value: stats.trashCount,
      icon: "fa-trash",
      color: "#f87171",
      subtitle: "Deleted items",
    },
    {
      title: "Users",
      value: stats.users,
      icon: "fa-users",
      color: "#34d399",
      subtitle: "Registered",
    },
  ];

  return (
    <div style={{padding: "24px"}}>
      <div style={{marginBottom: "24px"}}>
        <h2 style={{color: "#e5e7eb", fontSize: "20px", fontWeight: 600, margin: 0}}>
          Dashboard
        </h2>
        <p style={{color: "#6b7280", fontSize: "14px", margin: "4px 0 0 0"}}>
          Overview of your reMarkable Cloud
        </p>
      </div>

      {loading ? (
        <div style={{textAlign: "center", padding: "40px", color: "#6b7280"}}>
          Loading stats...
        </div>
      ) : (
        <>
          {/* Stats cards */}
          <div style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))",
            gap: "16px",
            marginBottom: "24px",
          }}>
            {cards.map((card, i) => (
              <div key={i} className="card" style={{marginBottom: 0, padding: "20px"}}>
                <div style={{display: "flex", alignItems: "center", justifyContent: "space-between"}}>
                  <div>
                    <div style={{fontSize: "12px", color: "#6b7280", textTransform: "uppercase", fontWeight: 600, letterSpacing: "0.5px"}}>
                      {card.title}
                    </div>
                    <div style={{fontSize: "32px", fontWeight: 700, color: "#e5e7eb", marginTop: "4px"}}>
                      {card.value}
                    </div>
                    <div style={{fontSize: "12px", color: "#6b7280", marginTop: "2px"}}>
                      {card.subtitle}
                    </div>
                  </div>
                  <div style={{
                    width: "48px",
                    height: "48px",
                    borderRadius: "8px",
                    background: `${card.color}15`,
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "center",
                    fontSize: "20px",
                    color: card.color,
                  }}>
                    <i className={`fas ${card.icon}`}></i>
                  </div>
                </div>
              </div>
            ))}
          </div>

          {/* System status */}
          <div className="card" style={{marginBottom: 0}}>
            <div className="card-header">
              <span className="card-title">System Status</span>
              <span className="badge badge-success">Operational</span>
            </div>
            <div style={{display: "flex", flexDirection: "column", gap: "12px"}}>
              <div style={{display: "flex", justifyContent: "space-between", alignItems: "center", padding: "8px 0", borderBottom: "1px solid #1f2937"}}>
                <span style={{color: "#9ca3af", fontSize: "14px"}}>
                  <i className="fas fa-sync-alt" style={{marginRight: "8px", color: "#34d399"}}></i>
                  Device Sync
                </span>
                <span className="badge badge-success">Active</span>
              </div>
              <div style={{display: "flex", justifyContent: "space-between", alignItems: "center", padding: "8px 0", borderBottom: "1px solid #1f2937"}}>
                <span style={{color: "#9ca3af", fontSize: "14px"}}>
                  <i className="fas fa-shield-alt" style={{marginRight: "8px", color: "#58a6ff"}}></i>
                  SSO Authentication
                </span>
                <span className="badge badge-success">Enabled</span>
              </div>
              <div style={{display: "flex", justifyContent: "space-between", alignItems: "center", padding: "8px 0"}}>
                <span style={{color: "#9ca3af", fontSize: "14px"}}>
                  <i className="fas fa-database" style={{marginRight: "8px", color: "#fbbf24"}}></i>
                  Database
                </span>
                <span className="badge badge-success">Connected</span>
              </div>
            </div>
          </div>
        </>
      )}
    </div>
  );
};

export default Home;