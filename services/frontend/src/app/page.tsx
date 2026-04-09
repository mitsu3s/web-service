"use client";

import { useCallback, useDeferredValue, useEffect, useState } from "react";

const API = process.env.NEXT_PUBLIC_API_URL ?? "/api";

type Project = {
  id: number;
  name: string;
  description: string;
  owner_user_id: number;
  workflow_profile: string;
  role: string;
  created_at: string;
};

type Task = {
  id: number;
  project_id: number;
  title: string;
  description: string;
  status: string;
  created_at: string;
  updated_at: string;
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

type WorkflowDefinition = {
  name: string;
  default_status: string;
  statuses: string[];
  transitions: Record<string, string[]>;
  terminal_statuses: string[];
};

type SearchResult = {
  task_id: number;
  project_id: number;
  user_id: number;
  title: string;
  description: string;
  status: string;
  event_type: string;
  occurred_at: string;
  score: number;
};

type SearchResponse = {
  query: string;
  total: number;
  results: SearchResult[];
};

type DashboardResponse = {
  projects: Project[];
  selected_project?: Project;
  tasks: Task[];
  activity: Activity[];
  workflow?: WorkflowDefinition;
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

function formatStatusLabel(status: string): string {
  return status
    .split("_")
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ");
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

function statusesForTask(workflow: WorkflowDefinition | null, currentStatus: string): string[] {
  if (!workflow) {
    return [currentStatus];
  }

  const allowed = new Set([currentStatus, ...(workflow.transitions[currentStatus] ?? [])]);
  return workflow.statuses.filter((status) => allowed.has(status));
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

function TaskBoard({ token, onLogout }: { token: string; onLogout: () => void }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [selectedProjectId, setSelectedProjectId] = useState<number | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [activities, setActivities] = useState<Activity[]>([]);
  const [workflow, setWorkflow] = useState<WorkflowDefinition | null>(null);
  const [newTaskTitle, setNewTaskTitle] = useState("");
  const [newTaskStatus, setNewTaskStatus] = useState("");
  const [newProjectName, setNewProjectName] = useState("");
  const [searchQuery, setSearchQuery] = useState("");
  const [searchResults, setSearchResults] = useState<SearchResult[]>([]);
  const [searchTotal, setSearchTotal] = useState(0);
  const [searchLoading, setSearchLoading] = useState(false);
  const [notification, setNotification] = useState<string | null>(null);
  const [error, setError] = useState("");

  const deferredSearchQuery = useDeferredValue(searchQuery);
  const selectedProject = projects.find((project) => project.id === selectedProjectId) ?? null;

  const loadDashboard = useCallback(async (projectId?: number | null) => {
    const effectiveProjectId = projectId ?? selectedProjectId;
    const query = effectiveProjectId ? `?project_id=${effectiveProjectId}&activity_limit=12` : "?activity_limit=12";
    let data = await apiFetch<DashboardResponse>(`/dashboard${query}`, {}, token);

    if (data.projects.length === 0) {
      await apiFetch<Project>(
        "/projects",
        {
          method: "POST",
          body: JSON.stringify({ name: "Personal Workspace", description: "Default project", workflow_profile: "team-kanban" }),
        },
        token,
      );
      data = await apiFetch<DashboardResponse>("/dashboard?activity_limit=12", {}, token);
    }

    setProjects(data.projects ?? []);
    setTasks(data.tasks ?? []);
    setActivities(data.activity ?? []);
    setWorkflow(data.workflow ?? null);

    const nextSelectedProjectId = data.selected_project?.id ?? data.projects[0]?.id ?? null;
    setSelectedProjectId((current) => (current === nextSelectedProjectId ? current : nextSelectedProjectId));
    setNewTaskStatus((current) => {
      if (!data.workflow?.statuses?.length) {
        return "";
      }
      if (current && data.workflow.statuses.includes(current)) {
        return current;
      }
      return data.workflow.default_status;
    });
  }, [selectedProjectId, token]);

  const loadSearch = useCallback(async () => {
    if (!selectedProjectId || !deferredSearchQuery.trim()) {
      setSearchResults([]);
      setSearchTotal(0);
      setSearchLoading(false);
      return;
    }

    setSearchLoading(true);
    try {
      const data = await apiFetch<SearchResponse>(
        `/search/tasks?project_id=${selectedProjectId}&limit=8&q=${encodeURIComponent(deferredSearchQuery.trim())}`,
        {},
        token,
      );
      setSearchResults(data.results ?? []);
      setSearchTotal(data.total ?? 0);
    } finally {
      setSearchLoading(false);
    }
  }, [deferredSearchQuery, selectedProjectId, token]);

  useEffect(() => {
    loadDashboard().catch((err) => setError(err instanceof Error ? err.message : "failed to load dashboard"));
  }, [loadDashboard]);

  useEffect(() => {
    loadSearch().catch((err) => setError(err instanceof Error ? err.message : "failed to search tasks"));
  }, [loadSearch]);

  useEffect(() => {
    const es = new EventSource(`${API}/notifications/events?token=${encodeURIComponent(token)}`);
    es.addEventListener("task-event", (event) => {
      const data = JSON.parse((event as MessageEvent).data) as TaskEvent;
      setNotification(`Task ${humanizeEvent(data.type)}: ${data.title}`);

      if (!selectedProjectId || data.project_id === selectedProjectId) {
        loadDashboard().catch(() => {});
        loadSearch().catch(() => {});
      }

      window.setTimeout(() => setNotification(null), 3000);
    });
    es.onerror = () => setNotification("Realtime stream disconnected. Retrying...");
    return () => es.close();
  }, [token, selectedProjectId, loadDashboard, loadSearch]);

  const createProject = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newProjectName.trim()) return;

    try {
      const created = await apiFetch<Project>(
        "/projects",
        {
          method: "POST",
          body: JSON.stringify({ name: newProjectName, workflow_profile: "team-kanban" }),
        },
        token,
      );
      setNewProjectName("");
      setSelectedProjectId(created.id);
      setError("");
      await loadDashboard(created.id);
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
          body: JSON.stringify({ title: newTaskTitle, project_id: selectedProjectId, status: newTaskStatus }),
        },
        token,
      );
      setNewTaskTitle("");
      setNewTaskStatus(workflow?.default_status ?? "");
      setError("");
      await loadDashboard();
      await loadSearch();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to create task");
    }
  };

  const updateStatus = async (id: number, status: string) => {
    try {
      await apiFetch(`/tasks/${id}`, { method: "PUT", body: JSON.stringify({ status }) }, token);
      setError("");
      await loadDashboard();
      await loadSearch();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to update task");
    }
  };

  const deleteTask = async (id: number) => {
    try {
      await apiFetch(`/tasks/${id}`, { method: "DELETE" }, token);
      setError("");
      await loadDashboard();
      await loadSearch();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to delete task");
    }
  };

  return (
    <div className="container app-shell">
      <header>
        <div>
          <h1>DevBoard</h1>
          <p className="subtitle">
            `web-bff` is now a thin edge. Reads flow through `board-service`, writes through `task-orchestrator-service`,
            and policy flows through `access-service`, `membership-service`, and `workflow-service`.
          </p>
        </div>
        <button className="btn-sm" onClick={onLogout}>
          Logout
        </button>
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
          <input placeholder="New project name" value={newProjectName} onChange={(e) => setNewProjectName(e.target.value)} />
          <button type="submit" className="btn-sm">
            Add Project
          </button>
        </form>
      </section>

      {error && <p className="error board-error">{error}</p>}

      <div className="board-grid">
        <section className="panel">
          <div className="panel-heading">
            <div>
              <h2>{selectedProject?.name ?? "Project"}</h2>
              <p>
                {selectedProject
                  ? `${selectedProject.role} role · ${selectedProject.workflow_profile} workflow · ${tasks.length} task(s)`
                  : "Select a project to begin"}
              </p>
            </div>
          </div>

          {workflow && (
            <div className="workflow-summary">
              <div>
                <strong>{workflow.name}</strong>
                <span>Default: {formatStatusLabel(workflow.default_status)}</span>
              </div>
              <div className="workflow-statuses">
                {workflow.statuses.map((status) => (
                  <span key={status} className={`status status-${status}`}>
                    {formatStatusLabel(status)}
                  </span>
                ))}
              </div>
            </div>
          )}

          <form className="task-form" onSubmit={createTask}>
            <input
              placeholder={selectedProjectId ? "New task title..." : "Create a project first"}
              value={newTaskTitle}
              onChange={(e) => setNewTaskTitle(e.target.value)}
              disabled={!selectedProjectId}
            />
            <select
              className="status-select"
              value={newTaskStatus}
              onChange={(e) => setNewTaskStatus(e.target.value)}
              disabled={!selectedProjectId || !workflow}
            >
              {(workflow?.statuses ?? []).map((status) => (
                <option key={status} value={status}>
                  {formatStatusLabel(status)}
                </option>
              ))}
            </select>
            <button type="submit" className="btn-primary task-submit" disabled={!selectedProjectId}>
              Add Task
            </button>
          </form>

          <div className="task-list">
            {tasks.length === 0 && <p className="empty">No tasks in this project yet.</p>}
            {tasks.map((task) => (
              <div className="task-card" key={task.id}>
                <div className="task-main">
                  <span className="title">{task.title}</span>
                  <span className={`status status-${task.status}`}>{formatStatusLabel(task.status)}</span>
                </div>
                <div className="task-actions">
                  <select className="status-select" value={task.status} onChange={(e) => updateStatus(task.id, e.target.value)}>
                    {statusesForTask(workflow, task.status).map((status) => (
                      <option key={status} value={status}>
                        {formatStatusLabel(status)}
                      </option>
                    ))}
                  </select>
                  <button className="btn-sm btn-danger" onClick={() => deleteTask(task.id)}>
                    Delete
                  </button>
                </div>
              </div>
            ))}
          </div>
        </section>

        <div className="side-stack">
          <aside className="panel activity-panel">
            <div className="panel-heading">
              <div>
                <h2>Activity Feed</h2>
                <p>`activity-service` stores RabbitMQ fan-out events and validates project access on reads.</p>
              </div>
            </div>

            <div className="activity-list">
              {activities.length === 0 && <p className="empty">No activity yet.</p>}
              {activities.map((activity) => (
                <div key={activity.event_id} className="activity-item">
                  <strong>{activity.title}</strong>
                  <span>
                    {humanizeEvent(activity.event_type)} · {formatStatusLabel(activity.status)}
                  </span>
                  <time>{new Date(activity.occurred_at).toLocaleString()}</time>
                </div>
              ))}
            </div>
          </aside>

          <aside className="panel search-panel">
            <div className="panel-heading">
              <div>
                <h2>Search</h2>
                <p>`board-service` and `search-service` both sit on the sync path for project-scoped search.</p>
              </div>
            </div>

            <input
              className="search-input"
              placeholder={selectedProjectId ? "Search task title or description" : "Select a project first"}
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              disabled={!selectedProjectId}
            />

            {searchLoading && <p className="search-meta">Searching...</p>}
            {!searchLoading && deferredSearchQuery.trim() && <p className="search-meta">{searchTotal} result(s)</p>}

            <div className="search-results">
              {!deferredSearchQuery.trim() && <p className="empty">Search results appear here.</p>}
              {deferredSearchQuery.trim() && !searchLoading && searchResults.length === 0 && (
                <p className="empty">No matching tasks.</p>
              )}
              {searchResults.map((result) => (
                <div key={`${result.task_id}-${result.occurred_at}`} className="search-item">
                  <div className="search-item-head">
                    <strong>{result.title}</strong>
                    <span className={`status status-${result.status}`}>{formatStatusLabel(result.status)}</span>
                  </div>
                  {result.description && <p>{result.description}</p>}
                  <time>{new Date(result.occurred_at).toLocaleString()}</time>
                </div>
              ))}
            </div>
          </aside>
        </div>
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
