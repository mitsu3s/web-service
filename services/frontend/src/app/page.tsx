"use client";

import { useState, useEffect, useCallback } from "react";

const API = process.env.NEXT_PUBLIC_API_URL ?? "/api";

type Task = {
  id: number;
  title: string;
  description: string;
  status: "pending" | "in_progress" | "done";
  created_at: string;
};

// ---- API helpers -----------------------------------------------------------

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

// ---- Auth screen -----------------------------------------------------------

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
          <button className="link-btn" onClick={() => { setMode(mode === "login" ? "register" : "login"); setError(""); }}>
            {mode === "login" ? "Register" : "Login"}
          </button>
        </p>
      </div>
    </div>
  );
}

// ---- Task board ------------------------------------------------------------

function TaskBoard({ token, onLogout }: { token: string; onLogout: () => void }) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [newTitle, setNewTitle] = useState("");
  const [notification, setNotification] = useState<string | null>(null);

  const fetchTasks = useCallback(async () => {
    const data = await apiFetch<Task[]>("/tasks", {}, token);
    setTasks(data ?? []);
  }, [token]);

  useEffect(() => {
    fetchTasks();
  }, [fetchTasks]);

  // SSE for real-time notifications
  useEffect(() => {
    const es = new EventSource(`${API}/notifications/events`, {});
    es.addEventListener("task-event", (e) => {
      const data = JSON.parse(e.data);
      setNotification(`Event: ${data.event}`);
      fetchTasks();
      setTimeout(() => setNotification(null), 3000);
    });
    return () => es.close();
  }, [fetchTasks]);

  const createTask = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newTitle.trim()) return;
    await apiFetch("/tasks", { method: "POST", body: JSON.stringify({ title: newTitle }) }, token);
    setNewTitle("");
    fetchTasks();
  };

  const updateStatus = async (id: number, status: Task["status"]) => {
    await apiFetch(`/tasks/${id}`, { method: "PUT", body: JSON.stringify({ status }) }, token);
    fetchTasks();
  };

  const deleteTask = async (id: number) => {
    await apiFetch(`/tasks/${id}`, { method: "DELETE" }, token);
    fetchTasks();
  };

  return (
    <div className="container">
      <header>
        <h1>DevBoard</h1>
        <button className="btn-sm" onClick={onLogout}>Logout</button>
      </header>

      <form className="task-form" onSubmit={createTask}>
        <input
          placeholder="New task title..."
          value={newTitle}
          onChange={(e) => setNewTitle(e.target.value)}
        />
        <button type="submit" className="btn-primary" style={{ width: "auto" }}>Add</button>
      </form>

      <div className="task-list">
        {tasks.length === 0 && <p className="empty">No tasks yet. Add one above!</p>}
        {tasks.map((t) => (
          <div className="task-card" key={t.id}>
            <span className="title">{t.title}</span>
            <select
              className="status-select"
              value={t.status}
              onChange={(e) => updateStatus(t.id, e.target.value as Task["status"])}
            >
              <option value="pending">Pending</option>
              <option value="in_progress">In Progress</option>
              <option value="done">Done</option>
            </select>
            <span className={`status status-${t.status}`}>{t.status}</span>
            <button className="btn-sm btn-danger" onClick={() => deleteTask(t.id)}>✕</button>
          </div>
        ))}
      </div>

      {notification && <div className="notification-bar">{notification}</div>}
    </div>
  );
}

// ---- App -------------------------------------------------------------------

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
