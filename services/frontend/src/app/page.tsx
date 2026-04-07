"use client";

import { useCallback, useEffect, useState } from "react";

const API = process.env.NEXT_PUBLIC_API_URL ?? "/api";

type Project = {
  id: number;
  name: string;
  description: string;
  owner_user_id: number;
  created_at: string;
};

type Task = {
  id: number;
  project_id: number;
  title: string;
  description: string;
  status: "pending" | "in_progress" | "done";
  created_at: string;
};

type Activity = {
  id: number;
  event_id: string;
  task_id: number;
  project_id: number;
  user_id: number;
  event_type: string;
  title: string;
  description: string;
  status: string;
  occurred_at: string;
};

type TaskEvent = {
  event_id: string;
  type: string;
  task_id: number;
  project_id: number;
  title: string;
  status: string;
};

async function apiFetch<T>(path: string, options: RequestInit = {}, token?: string): Promise<T> {
  const headers: HeadersInit = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;

  const res = await fetch(`${API}${path}`, { ...options, headers });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error ?? "request failed");
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

function AuthScreen({ onLogin }: { onLogin: (token: string) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    try {
      if (mode === "register") {
        await apiFetch("/auth/register", {
          method: "POST",
          body: JSON.stringify({ email, password }),
        });
        setMode("login");
        return;
      }

      const data = await apiFetch<{ token: string }>("/auth/login", {
        method: "POST",
        body: JSON.stringify({ email, password }),
      });
      localStorage.setItem("token", data.token);
      onLogin(data.token);
    } catch (err) {
      setError(err instanceof Error ? err.message : "error");
    }
  };

  return (
    <div className="container">
      <div className="auth-form">
        <h2>{mode === "login" ? "Login" : "Register"}</h2>
        <form onSubmit={submit}>
          <div className="field">
            <label>Email</label>
            <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
          </div>
          <div className="field">
            <label>Password</label>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          </div>
          {error && <p className="error">{error}</p>}
          <button type="submit" className="btn-primary">
            {mode === "login" ? "Login" : "Create Account"}
          </button>
        </form>
        <p style={{ marginTop: "1rem", textAlign: "center", fontSize: "0.875rem" }}>
          {mode === "login" ? "No account? " : "Have an account? "}
          <button
            className="link-btn"
            onClick={() => {
              setMode(mode === "login" ? "register" : "login");
              setError("");
            }}
          >
            {mode === "login" ? "Register" : "Login"}
          </button>
        </p>
      </div>
    </div>
  );
}

function humanizeEvent(eventType: string): string {
  switch (eventType) {
    case "task.created":
      return "created";
    case "task.updated":
      return "updated";
    case "task.deleted":
      return "deleted";
    default:
      return eventType;
  }
}

function TaskBoard({ token, onLogout }: { token: string; onLogout: () => void }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [selectedProjectId, setSelectedProjectId] = useState<number | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [activities, setActivities] = useState<Activity[]>([]);
  const [newTaskTitle, setNewTaskTitle] = useState("");
  const [newProjectName, setNewProjectName] = useState("");
  const [notification, setNotification] = useState<string | null>(null);
  const [error, setError] = useState("");

  const selectedProject = projects.find((project) => project.id === selectedProjectId) ?? null;

  const loadProjects = useCallback(async () => {
    let data = await apiFetch<Project[]>("/projects", {}, token);
    if (data.length === 0) {
      const created = await apiFetch<Project>(
        "/projects",
        {
          method: "POST",
          body: JSON.stringify({ name: "Personal Workspace", description: "Default project" }),
        },
        token,
      );
      data = [created];
    }

    setProjects(data);
    setSelectedProjectId((current) => (current && data.some((project) => project.id === current) ? current : data[0]?.id ?? null));
  }, [token]);

  const loadTasks = useCallback(async () => {
    if (!selectedProjectId) {
      setTasks([]);
      return;
    }
    const data = await apiFetch<Task[]>(`/tasks?project_id=${selectedProjectId}`, {}, token);
    setTasks(data ?? []);
  }, [selectedProjectId, token]);

  const loadActivity = useCallback(async () => {
    if (!selectedProjectId) {
      setActivities([]);
      return;
    }
    const data = await apiFetch<Activity[]>(`/activity?project_id=${selectedProjectId}&limit=12`, {}, token);
    setActivities(data ?? []);
  }, [selectedProjectId, token]);

  useEffect(() => {
    loadProjects().catch((err) => setError(err instanceof Error ? err.message : "failed to load projects"));
  }, [loadProjects]);

  useEffect(() => {
    loadTasks().catch((err) => setError(err instanceof Error ? err.message : "failed to load tasks"));
    loadActivity().catch((err) => setError(err instanceof Error ? err.message : "failed to load activity"));
  }, [loadTasks, loadActivity]);

  useEffect(() => {
    const es = new EventSource(`${API}/notifications/events?token=${encodeURIComponent(token)}`);
    es.addEventListener("task-event", (event) => {
      const data = JSON.parse((event as MessageEvent).data) as TaskEvent;
      setNotification(`Task ${humanizeEvent(data.type)}: ${data.title}`);

      if (!selectedProjectId || data.project_id === selectedProjectId) {
        loadTasks().catch(() => {});
        loadActivity().catch(() => {});
      }

      window.setTimeout(() => setNotification(null), 3000);
    });
    es.onerror = () => setNotification("Realtime stream disconnected. Retrying...");
    return () => es.close();
  }, [token, selectedProjectId, loadTasks, loadActivity]);

  const createProject = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newProjectName.trim()) return;

    try {
      const created = await apiFetch<Project>(
        "/projects",
        {
          method: "POST",
          body: JSON.stringify({ name: newProjectName }),
        },
        token,
      );
      setProjects((current) => [...current, created]);
      setSelectedProjectId(created.id);
      setNewProjectName("");
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to create project");
    }
  };

  const createTask = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newTaskTitle.trim() || !selectedProjectId) return;

    try {
      await apiFetch(
        "/tasks",
        {
          method: "POST",
          body: JSON.stringify({ title: newTaskTitle, project_id: selectedProjectId }),
        },
        token,
      );
      setNewTaskTitle("");
      setError("");
      await loadTasks();
      await loadActivity();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to create task");
    }
  };

  const updateStatus = async (id: number, status: Task["status"]) => {
    try {
      await apiFetch(`/tasks/${id}`, { method: "PUT", body: JSON.stringify({ status }) }, token);
      setError("");
      await loadTasks();
      await loadActivity();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to update task");
    }
  };

  const deleteTask = async (id: number) => {
    try {
      await apiFetch(`/tasks/${id}`, { method: "DELETE" }, token);
      setError("");
      await loadTasks();
      await loadActivity();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to delete task");
    }
  };

  return (
    <div className="container app-shell">
      <header>
        <div>
          <h1>DevBoard</h1>
          <p className="subtitle">Projects drive task ownership. Task events fan out to activity, notifications, and search indexing.</p>
        </div>
        <button className="btn-sm" onClick={onLogout}>Logout</button>
      </header>

      <section className="panel control-panel">
        <div className="project-picker">
          <label>Active Project</label>
          <select
            className="status-select"
            value={selectedProjectId ?? ""}
            onChange={(e) => setSelectedProjectId(Number(e.target.value))}
          >
            {projects.map((project) => (
              <option key={project.id} value={project.id}>
                {project.name}
              </option>
            ))}
          </select>
        </div>

        <form className="project-form" onSubmit={createProject}>
          <input
            placeholder="New project name"
            value={newProjectName}
            onChange={(e) => setNewProjectName(e.target.value)}
          />
          <button type="submit" className="btn-sm">Add Project</button>
        </form>
      </section>

      {error && <p className="error board-error">{error}</p>}

      <div className="board-grid">
        <section className="panel">
          <div className="panel-heading">
            <div>
              <h2>{selectedProject?.name ?? "Project"}</h2>
              <p>{selectedProject ? `${tasks.length} task(s) in view` : "Select a project to begin"}</p>
            </div>
          </div>

          <form className="task-form" onSubmit={createTask}>
            <input
              placeholder={selectedProjectId ? "New task title..." : "Create a project first"}
              value={newTaskTitle}
              onChange={(e) => setNewTaskTitle(e.target.value)}
              disabled={!selectedProjectId}
            />
            <button type="submit" className="btn-primary" style={{ width: "auto" }} disabled={!selectedProjectId}>
              Add Task
            </button>
          </form>

          <div className="task-list">
            {tasks.length === 0 && <p className="empty">No tasks in this project yet.</p>}
            {tasks.map((task) => (
              <div className="task-card" key={task.id}>
                <div className="task-main">
                  <span className="title">{task.title}</span>
                  <span className={`status status-${task.status}`}>{task.status}</span>
                </div>
                <div className="task-actions">
                  <select
                    className="status-select"
                    value={task.status}
                    onChange={(e) => updateStatus(task.id, e.target.value as Task["status"])}
                  >
                    <option value="pending">Pending</option>
                    <option value="in_progress">In Progress</option>
                    <option value="done">Done</option>
                  </select>
                  <button className="btn-sm btn-danger" onClick={() => deleteTask(task.id)}>Delete</button>
                </div>
              </div>
            ))}
          </div>
        </section>

        <aside className="panel activity-panel">
          <div className="panel-heading">
            <div>
              <h2>Activity Feed</h2>
              <p>Stored asynchronously by `activity-service` via RabbitMQ.</p>
            </div>
          </div>

          <div className="activity-list">
            {activities.length === 0 && <p className="empty">No activity yet.</p>}
            {activities.map((activity) => (
              <div key={activity.event_id} className="activity-item">
                <strong>{activity.title}</strong>
                <span>{humanizeEvent(activity.event_type)}</span>
                <time>{new Date(activity.occurred_at).toLocaleString()}</time>
              </div>
            ))}
          </div>
        </aside>
      </div>

      {notification && <div className="notification-bar">{notification}</div>}
    </div>
  );
}

export default function App() {
  const [token, setToken] = useState<string | null>(null);

  useEffect(() => {
    const stored = localStorage.getItem("token");
    if (stored) setToken(stored);
  }, []);

  const logout = () => {
    localStorage.removeItem("token");
    setToken(null);
  };

  if (!token) return <AuthScreen onLogin={setToken} />;
  return <TaskBoard token={token} onLogout={logout} />;
}
