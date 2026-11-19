// Error boundary: a render error in one part shows a notice there instead of unmounting the app.
import { Component, type ReactNode } from "react";

interface P {
  what: string;
  children: ReactNode;
  className?: string;
  /** A new value clears the error, for example the route path. */
  resetKey?: string;
}
export class Boundary extends Component<P, { err: Error | null }> {
  state = { err: null as Error | null };
  static getDerivedStateFromError(err: Error) {
    return { err };
  }
  componentDidUpdate(prev: P) {
    if (this.state.err && prev.resetKey !== this.props.resetKey) this.setState({ err: null });
  }
  componentDidCatch(err: Error) {
    console.error(`${this.props.what} failed to render`, err);
  }
  render() {
    if (!this.state.err) return this.props.children;
    return (
      <div className={`werr ${this.props.className || ""}`} role="alert">
        <b>{this.props.what} could not be shown.</b> <span className="mono xs">{this.state.err.message}</span>{" "}
        <button className="btn sm" onClick={() => this.setState({ err: null })}>
          Try again
        </button>
      </div>
    );
  }
}
